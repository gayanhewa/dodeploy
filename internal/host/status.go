package host

import (
	"context"
	"fmt"

	"github.com/gayanhewa/dodeploy/internal/appspec"
)

// AppStatus is the observed state of one app on a host.
type AppStatus struct {
	Name    string
	Domain  string
	Port    int
	Unit    string // systemd's answer: active, inactive, failed
	Healthy bool
	// NotDeployed means the unit does not exist at all, as opposed to existing
	// and being stopped: a distinction worth keeping, because one is "never
	// deployed" and the other is "broken".
	NotDeployed bool
}

// Status reports the state of the host and the apps expected on it.
//
// The health check is made from the host against the loopback port, so a failure
// points at the app rather than at DNS or TLS.
func (h *Host) Status(ctx context.Context, specs []*appspec.Spec) error {
	if _, err := h.DigitalOcean(); err != nil {
		return err
	}
	if err := h.Resolve(ctx); err != nil {
		return err
	}

	fmt.Fprintf(h.Out, "host %s\n", h.Name)
	fmt.Fprintf(h.Out, "  address   %s\n", h.IP)
	fmt.Fprintf(h.Out, "  droplet   id %d, %s, %s\n", h.Droplet.ID, h.Droplet.SizeSlug, h.Droplet.Region.Slug)
	fmt.Fprintf(h.Out, "  status    %s\n", h.Droplet.Status)
	if h.Config.ReservedIP {
		reserved, err := h.reservedIPFor(ctx)
		if err == nil {
			fmt.Fprintf(h.Out, "  reserved  %s\n", orNone(reserved))
		}
	}

	sshOK := h.Remote.RunTolerant(ctx, "true")
	fmt.Fprintf(h.Out, "  ssh       %s\n", yesNo(sshOK, "reachable", "unreachable"))

	apps := h.sharedApps(specs)
	if len(apps) == 0 {
		return nil
	}

	fmt.Fprintln(h.Out)
	fmt.Fprintf(h.Out, "  %-22s %-34s %-8s %-10s %s\n", "APP", "DOMAIN", "PORT", "UNIT", "HEALTH")
	for _, spec := range apps {
		st := AppStatus{Name: spec.Name, Domain: spec.Domain, Port: spec.Port}

		if !sshOK {
			st.Unit = "unknown"
			printStatus(h, st)
			continue
		}

		unit := spec.ServiceName()
		if !h.Remote.RunTolerant(ctx, "systemctl list-unit-files --quiet "+unit+".service >/dev/null 2>&1 || systemctl status "+unit+" >/dev/null 2>&1") {
			st.NotDeployed = true
			st.Unit = "-"
			printStatus(h, st)
			continue
		}

		state, err := h.Remote.Output(ctx, "systemctl is-active "+unit+" 2>/dev/null || true")
		if err != nil {
			state = "error"
		}
		st.Unit = state
		st.Healthy = h.Remote.RunTolerant(ctx, fmt.Sprintf(
			"curl -fsS --max-time 5 %s >/dev/null 2>&1",
			quote(fmt.Sprintf("http://127.0.0.1:%d%s", spec.Port, spec.Health))))
		printStatus(h, st)
	}
	return nil
}

func printStatus(h *Host, st AppStatus) {
	unit := st.Unit
	health := "unhealthy"
	switch {
	case st.NotDeployed:
		unit = "not deployed"
		health = "-"
	case st.Healthy:
		health = "ok"
	}
	fmt.Fprintf(h.Out, "  %-22s %-34s %-8d %-10s %s\n", st.Name, st.Domain, st.Port, unit, health)
}

func (h *Host) reservedIPFor(ctx context.Context) (string, error) {
	client, err := h.DigitalOcean()
	if err != nil {
		return "", err
	}
	return client.ReservedIPForDroplet(ctx, h.Name)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func yesNo(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
