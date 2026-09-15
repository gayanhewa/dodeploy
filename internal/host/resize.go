package host

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gayanhewa/dodeploy/internal/doapi"
)

// ResizeOptions describes a resize.
type ResizeOptions struct {
	// Size is the target size slug, e.g. s-1vcpu-1gb.
	Size string
	// Disk also grows the disk. This is permanent: DigitalOcean can grow a disk
	// but never shrink it, so it cannot be undone by resizing back down.
	Disk bool
	// Snapshot captures a droplet image first. Backups are off by default on a
	// cheap droplet and this is the only undo a disk resize has.
	Snapshot bool
	// AssumeYes skips the confirmation prompt.
	AssumeYes bool
	// Timeout bounds each individual wait.
	Timeout time.Duration
}

// Resize changes the host's droplet size.
//
// The droplet is powered off for the duration. DigitalOcean requires it, and the
// apps come back on their own because their units are enabled and Caddy is
// enabled, so nothing needs restarting by hand afterwards.
func (h *Host) Resize(ctx context.Context, opts ResizeOptions) error {
	if strings.TrimSpace(opts.Size) == "" {
		return fmt.Errorf("a target size is required, for example --size s-1vcpu-1gb\n" +
			"      run `dodeploy sizes` to see the options")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Minute
	}

	if err := h.Resolve(ctx); err != nil {
		return err
	}
	client, err := h.DigitalOcean()
	if err != nil {
		return err
	}

	current, err := client.SizeBySlug(ctx, h.Droplet.SizeSlug)
	if err != nil {
		return err
	}
	target, err := client.SizeBySlug(ctx, opts.Size)
	if err != nil {
		return err
	}

	if err := h.validateResize(current, target, opts); err != nil {
		return err
	}

	if err := h.confirmResize(current, target, opts); err != nil {
		return err
	}

	if opts.Snapshot {
		name := fmt.Sprintf("%s-pre-resize-%s", h.Name, time.Now().UTC().Format("20060102-1504"))
		h.Logf("taking snapshot %s (remember to delete it later; it is billed monthly)", name)
		action, err := client.Snapshot(ctx, h.Droplet.ID, name)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		if err := client.WaitForAction(ctx, h.Droplet.ID, action.ID, opts.Timeout); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		h.Logf("snapshot captured")
	}

	// Power off: required by DigitalOcean, and the only state a disk resize can
	// start from.
	h.Logf("powering off %s (the apps will be down for a few minutes)", h.Name)
	if err := h.runAction(ctx, client, opts.Timeout, client.PowerOff, "power off"); err != nil {
		return err
	}
	if err := client.WaitForPowerState(ctx, h.Droplet.ID, "off", opts.Timeout); err != nil {
		return err
	}

	h.Logf("resizing to %s (disk=%v, %dGB -> %dGB, %dMB -> %dMB)", target.Slug, opts.Disk,
		current.Disk, target.Disk, current.Memory, target.Memory)
	resizeAction, err := client.Resize(ctx, h.Droplet.ID, target.Slug, opts.Disk)
	if err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	if err := client.WaitForAction(ctx, h.Droplet.ID, resizeAction.ID, opts.Timeout); err != nil {
		return fmt.Errorf("resize: %w", err)
	}

	h.Logf("powering on")
	if err := h.runAction(ctx, client, opts.Timeout, client.PowerOn, "power on"); err != nil {
		return err
	}
	if err := client.WaitForPowerState(ctx, h.Droplet.ID, "active", opts.Timeout); err != nil {
		return err
	}

	// A resize rewrites the droplet's networking, so re-read it rather than
	// assuming the address survived.
	h.Logf("waiting for ssh")
	if err := h.checkSSH(ctx); err != nil {
		return err
	}

	resized, err := client.DropletByName(ctx, h.Name)
	if err != nil {
		return err
	}
	h.Logf("now %s: %dGB disk, %dMB memory", resized.SizeSlug, target.Disk, target.Memory)

	fmt.Fprintln(h.Out)
	h.Logf("checking the apps came back")
	return h.Status(ctx, nil)
}

// runAction issues a droplet action and waits for it.
func (h *Host) runAction(
	ctx context.Context,
	client *doapi.Client,
	timeout time.Duration,
	call func(context.Context, int) (*doapi.Action, error),
	label string,
) error {
	action, err := call(ctx, h.Droplet.ID)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := client.WaitForAction(ctx, h.Droplet.ID, action.ID, timeout); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// validateResize rejects anything DigitalOcean would refuse, with a message that
// says why rather than leaving the API's wording to explain it.
func (h *Host) validateResize(current, target *doapi.Size, opts ResizeOptions) error {
	if current.Slug == target.Slug {
		return fmt.Errorf("%s is already %s", h.Name, target.Slug)
	}
	if !target.Available {
		return fmt.Errorf("size %s is not currently available", target.Slug)
	}

	region := h.Droplet.Region.Slug
	available := false
	for _, r := range target.Regions {
		if r == region {
			available = true
			break
		}
	}
	if !available {
		return fmt.Errorf("size %s is not offered in %s", target.Slug, region)
	}

	// A disk can grow but never shrink, so a smaller disk is never valid and a
	// same-size disk is pointless.
	if target.Disk < current.Disk {
		return fmt.Errorf("size %s has a %dGB disk, smaller than the current %dGB; "+
			"a DigitalOcean disk can grow but never shrink", target.Slug, target.Disk, current.Disk)
	}
	if target.Memory < current.Memory {
		return fmt.Errorf("size %s has less memory (%dMB) than the current %dMB",
			target.Slug, target.Memory, current.Memory)
	}
	if !opts.Disk && target.Disk != current.Disk {
		return fmt.Errorf("size %s has a %dGB disk but the droplet has %dGB. "+
			"Pass --disk to grow the disk permanently, or pick a size with the same disk",
			target.Slug, target.Disk, current.Disk)
	}
	return nil
}

func (h *Host) confirmResize(current, target *doapi.Size, opts ResizeOptions) error {
	fmt.Fprintf(h.Out, "\n  host      %s (%s, %s)\n", h.Name, h.IP, h.Droplet.Region.Slug)
	fmt.Fprintf(h.Out, "  from      %s  %dGB disk, %dMB memory   $%.2f/mo\n",
		current.Slug, current.Disk, current.Memory, current.PriceMonthly)
	fmt.Fprintf(h.Out, "  to        %s  %dGB disk, %dMB memory   $%.2f/mo\n",
		target.Slug, target.Disk, target.Memory, target.PriceMonthly)
	fmt.Fprintf(h.Out, "  disk      %s\n", map[bool]string{
		true:  "growing (permanent: a DigitalOcean disk can never be shrunk again)",
		false: "unchanged",
	}[opts.Disk])
	fmt.Fprintf(h.Out, "  downtime  the droplet powers off for a few minutes\n")

	if opts.AssumeYes {
		fmt.Fprintln(h.Out)
		return nil
	}

	fmt.Fprintf(h.Out, "\nType %q to proceed: ", target.Slug)
	var typed string
	if _, err := fmt.Fscanln(h.In, &typed); err != nil {
		return fmt.Errorf("aborted")
	}
	if strings.TrimSpace(typed) != target.Slug {
		return fmt.Errorf("aborted")
	}
	fmt.Fprintln(h.Out)
	return nil
}

// AvailableSizes lists the sizes offered in the host's region, cheapest first,
// so a caller can choose without consulting the DigitalOcean console.
func (h *Host) AvailableSizes(ctx context.Context) ([]doapi.Size, error) {
	if err := h.Resolve(ctx); err != nil {
		return nil, err
	}
	client, err := h.DigitalOcean()
	if err != nil {
		return nil, err
	}

	// Read the region from the droplet itself: it is authoritative, whereas the
	// configuration may have been edited since the droplet was made.
	region := h.Droplet.Region.Slug

	all, err := client.Sizes(ctx)
	if err != nil {
		return nil, err
	}

	var out []doapi.Size
	for _, s := range all {
		if !s.Available || !strings.HasPrefix(s.Slug, "s-") {
			continue
		}
		for _, r := range s.Regions {
			if r == region {
				out = append(out, s)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PriceMonthly != out[j].PriceMonthly {
			return out[i].PriceMonthly < out[j].PriceMonthly
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}
