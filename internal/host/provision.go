package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gayanhewa/dodeploy/internal/appspec"
	"github.com/gayanhewa/dodeploy/internal/cloudinit"
	"github.com/gayanhewa/dodeploy/internal/dns"
	"github.com/gayanhewa/dodeploy/internal/doapi"
)

// ProvisionOptions adjusts what provisioning does.
type ProvisionOptions struct {
	// SkipDNS leaves DNS alone, for when records are managed elsewhere.
	SkipDNS bool
	// Timeout bounds the wait for first-boot.
	Timeout time.Duration
	// Recreate destroys an existing droplet first. Destructive.
	Recreate bool
}

// Provision creates the host, waits for it to become usable, and points every
// app's domain at it.
//
// Every step is idempotent: an existing droplet is reused, an existing reserved
// IP is reused, and DNS records are reconciled rather than duplicated. Running it
// again is the way to repair a half-finished setup.
func (h *Host) Provision(ctx context.Context, specs []*appspec.Spec, opts ProvisionOptions) error {
	if err := checkPorts(specs); err != nil {
		return err
	}
	if err := checkDomains(specs); err != nil {
		return err
	}

	client, err := h.DigitalOcean()
	if err != nil {
		return err
	}

	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Minute
	}

	if opts.Recreate {
		if err := h.destroy(ctx, client); err != nil {
			return err
		}
	}

	if err := h.ensureDroplet(ctx, client); err != nil {
		return err
	}
	if err := h.ensureReservedIP(ctx, client); err != nil {
		return err
	}
	if err := h.waitForBoot(ctx, opts.Timeout); err != nil {
		return err
	}

	if !opts.SkipDNS {
		if err := h.syncDNS(ctx, specs); err != nil {
			return err
		}
	}

	fmt.Fprintln(h.Out)
	h.Logf("host %s ready at %s", h.Name, h.IP)
	return nil
}

// destroy removes an existing droplet so it can be rebuilt.
func (h *Host) destroy(ctx context.Context, client *doapi.Client) error {
	droplet, err := client.DropletByName(ctx, h.Name)
	if errors.Is(err, doapi.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	h.Logf("destroying droplet %s (id %d)", h.Name, droplet.ID)
	if err := client.DeleteDroplet(ctx, droplet.ID); err != nil {
		return err
	}

	// Wait for it to disappear, otherwise the create that follows races it.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := client.DropletByName(ctx, h.Name); errors.Is(err, doapi.ErrNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("droplet %q still exists after 3 minutes", h.Name)
}

func (h *Host) ensureDroplet(ctx context.Context, client *doapi.Client) error {
	if droplet, err := client.DropletByName(ctx, h.Name); err == nil {
		h.Droplet = droplet
		h.Logf("droplet %s already exists (id %d)", h.Name, droplet.ID)
		return nil
	} else if !errors.Is(err, doapi.ErrNotFound) {
		return err
	}

	keys, err := client.ResolveSSHKeys(ctx, h.Config.SSHKeys)
	if err != nil {
		return err
	}
	userData, err := cloudinit.Base()
	if err != nil {
		return err
	}

	h.Logf("creating droplet %s (%s, %s, %s)", h.Name, h.Config.Size, h.Config.Region, h.Config.Image)
	h.Logf("authorising %d SSH key(s)", len(keys))

	droplet, err := client.CreateDroplet(ctx, doapi.CreateDropletRequest{
		Name:       h.Name,
		Region:     h.Config.Region,
		Size:       h.Config.Size,
		Image:      h.Config.Image,
		SSHKeys:    keys,
		UserData:   userData,
		IPv6:       true,
		Monitoring: h.Config.Monitoring,
	})
	if err != nil {
		return err
	}
	h.Droplet = droplet
	h.Logf("droplet created (id %d)", droplet.ID)
	return nil
}

// ensureReservedIP attaches a reserved address so the host's address survives a
// rebuild. Without one, DNS points at the anchor address, which changes.
func (h *Host) ensureReservedIP(ctx context.Context, client *doapi.Client) error {
	if !h.Config.ReservedIP {
		return nil
	}
	if h.Droplet == nil {
		return errors.New("droplet must exist before attaching a reserved IP")
	}

	existing, err := client.ReservedIPForDroplet(ctx, h.Name)
	if err != nil {
		return err
	}
	if existing != "" {
		h.IP = existing
		h.Logf("reserved IP %s already attached", existing)
		return nil
	}

	h.Logf("reserving an IP in %s", h.Config.Region)
	reserved, err := client.CreateReservedIP(ctx, h.Config.Region)
	if err != nil {
		return err
	}
	if err := client.AssignReservedIP(ctx, reserved.IP, h.Droplet.ID); err != nil {
		return err
	}
	h.IP = reserved.IP
	h.Logf("reserved and attached %s", reserved.IP)
	return nil
}

func (h *Host) waitForBoot(ctx context.Context, timeout time.Duration) error {
	if h.IP == "" {
		if err := h.Resolve(ctx); err != nil {
			return err
		}
	}

	h.Logf("waiting for first-boot setup (apt, caddy, go, swap) - a few minutes")
	if err := h.checkSSH(ctx); err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := h.Remote.WaitFor(waitCtx, "test -f /srv/apps/.provisioned", 10*time.Second); err != nil {
		return fmt.Errorf("%w\n      inspect with: ssh %s 'tail -50 /var/log/cloud-init-output.log'",
			err, h.Remote.Target())
	}
	h.Logf("first-boot setup finished")
	return nil
}

// checkSSH distinguishes "not reachable yet" from "the key is wrong", which are
// very different problems with the same symptom.
func (h *Host) checkSSH(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Minute)
	var lastErr error

	for time.Now().Before(deadline) {
		if _, err := h.Remote.Output(ctx, "true"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("cannot ssh to %s: %w", h.Remote.Target(), lastErr)
}

// syncDNS points every app's domain, and its www alias, at the host.
func (h *Host) syncDNS(ctx context.Context, specs []*appspec.Spec) error {
	provider, err := h.DNS()
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, spec := range specs {
		for _, domain := range spec.Domains() {
			if seen[domain] {
				continue
			}
			seen[domain] = true

			result, err := dns.EnsureA(ctx, provider, domain, h.IP, "600")
			if err != nil {
				if errors.Is(err, dns.ErrNotOptedIn) {
					return err
				}
				return fmt.Errorf("dns for %s: %w", domain, err)
			}
			for _, removed := range result.Removed {
				h.Logf("dns: removed shadowing %s on %s", removed, domain)
			}
			h.Logf("dns: %s", result)
		}
	}
	return nil
}
