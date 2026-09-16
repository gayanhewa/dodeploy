// Package host orchestrates provisioning and deploying against a machine.
//
// It is the only package that knows the shape of a host: one Caddy proxy owning
// the public ports, apps on loopback ports under /srv/apps, each with its own
// systemd unit and .env.
package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gayanhewa/dodeploy/internal/appspec"
	"github.com/gayanhewa/dodeploy/internal/caddy"
	"github.com/gayanhewa/dodeploy/internal/config"
	"github.com/gayanhewa/dodeploy/internal/dns"
	"github.com/gayanhewa/dodeploy/internal/doapi"
	"github.com/gayanhewa/dodeploy/internal/remote"
)

// RemoteRoot is where apps live on a host. Application state is kept inside each
// app's own directory but outside the synced source, because the source tree is
// mirrored with --delete.
const RemoteRoot = "/srv/apps"

// ServiceUser runs the apps.
const ServiceUser = "apps"

// Host is a configured machine.
type Host struct {
	Name   string
	Config config.Host
	cfg    *config.Config

	// Resolved by Resolve.
	Droplet *doapi.Droplet
	Remote  remote.Host

	// IP is the address DNS should point at: the reserved IP when there is one,
	// otherwise the droplet's anchor address.
	IP string

	Out io.Writer
	// In is read for confirmation prompts, so they can be driven in tests.
	In io.Reader
}

// New builds a Host from configuration. It performs no network calls, so
// commands that only need local information stay fast.
func New(cfg *config.Config, name string, out io.Writer) (*Host, error) {
	hostName, hc, err := cfg.Host(name)
	if err != nil {
		return nil, err
	}
	if hc.Region == "" || hc.Size == "" || hc.Image == "" {
		return nil, fmt.Errorf("host %q needs region, size and image in %s", hostName, cfg.Path())
	}
	return &Host{Name: hostName, Config: hc, cfg: cfg, Out: out, In: os.Stdin}, nil
}

// Logf prints progress.
func (h *Host) Logf(format string, args ...any) {
	fmt.Fprintf(h.Out, "==> "+format+"\n", args...)
}

// Warnf prints a non-fatal problem.
func (h *Host) Warnf(format string, args ...any) {
	fmt.Fprintf(h.Out, "warning: "+format+"\n", args...)
}

// DigitalOcean returns an API client, resolving the token on first use.
func (h *Host) DigitalOcean() (*doapi.Client, error) {
	token, err := h.cfg.DOToken()
	if err != nil {
		return nil, err
	}
	return doapi.New(token), nil
}

// DNS returns the DNS provider, resolving credentials on first use.
func (h *Host) DNS() (dns.Provider, error) {
	apiKey, secret, err := h.cfg.PorkbunCreds()
	if err != nil {
		return nil, err
	}
	return dns.NewPorkbun(apiKey, secret), nil
}

// Resolve finds the droplet and works out which address to use.
//
// A reserved IP is preferred over the droplet's anchor address. A droplet with a
// reserved IP attached has two public IPv4s and the API lists the anchor first;
// the anchor is also the one that changes if the droplet is ever rebuilt, which
// is the whole point of reserving an address.
func (h *Host) Resolve(ctx context.Context) error {
	if err := remote.CheckTools(); err != nil {
		return err
	}

	client, err := h.DigitalOcean()
	if err != nil {
		return err
	}

	droplet, err := client.DropletByName(ctx, h.Name)
	if err != nil {
		return err
	}
	h.Droplet = droplet

	ip, err := client.ReservedIPForDroplet(ctx, h.Name)
	if err != nil {
		return err
	}
	if ip == "" {
		ip = droplet.PublicIPv4()
	}
	if ip == "" {
		return fmt.Errorf("droplet %q has no public IPv4 address yet", h.Name)
	}
	h.IP = ip

	h.Remote = remote.Host{
		Address:  ip,
		User:     "deploy",
		Identity: h.Config.SSHIdentity,
	}
	return nil
}

// ensureResolved resolves the droplet unless it already has, so a command that
// samples in a loop pays for the lookup once rather than on every tick.
func (h *Host) ensureResolved(ctx context.Context) error {
	if h.Droplet != nil && h.IP != "" {
		return nil
	}
	return h.Resolve(ctx)
}

// RegionSlug is the region the host's droplet lives in.
func (h *Host) RegionSlug() string {
	if h.Droplet == nil {
		return ""
	}
	return h.Droplet.Region.Slug
}

// CurrentSizeSlug is the droplet's current size.
func (h *Host) CurrentSizeSlug(ctx context.Context) (string, error) {
	if h.Droplet == nil {
		if err := h.Resolve(ctx); err != nil {
			return "", err
		}
	}
	return h.Droplet.SizeSlug, nil
}

// AppDir is the app's directory on the host.
func (h *Host) AppDir(spec *appspec.Spec) string {
	return spec.RemoteDir(RemoteRoot)
}

// sites renders the proxy configuration for a set of apps.
func sites(specs []*appspec.Spec) []caddy.Site {
	out := make([]caddy.Site, 0, len(specs))
	for _, s := range specs {
		out = append(out, caddy.Site{
			Domain:      s.Domain,
			Aliases:     caddy.WithWWW(s.Domain, s.Aliases),
			Port:        s.Port,
			TLSInternal: s.TLS == "internal",
		})
	}
	return out
}

// checkPorts rejects two apps claiming the same loopback port, which would make
// one of them silently shadow the other.
func checkPorts(specs []*appspec.Spec) error {
	seen := map[int]string{}
	for _, s := range specs {
		if other, ok := seen[s.Port]; ok {
			return fmt.Errorf("apps %q and %q both claim port %d on this host", other, s.Name, s.Port)
		}
		seen[s.Port] = s.Name
	}
	return nil
}

// checkDomains rejects two apps claiming the same domain.
func checkDomains(specs []*appspec.Spec) error {
	seen := map[string]string{}
	for _, s := range specs {
		for _, d := range s.Domains() {
			if other, ok := seen[d]; ok {
				return fmt.Errorf("apps %q and %q both claim the domain %s", other, s.Name, d)
			}
			seen[d] = s.Name
		}
	}
	return nil
}

// sharedApps returns the apps on this host, given everything the caller handed
// in. Specs naming a different host are filtered out.
func (h *Host) sharedApps(specs []*appspec.Spec) []*appspec.Spec {
	var out []*appspec.Spec
	for _, s := range specs {
		if s.Host == "" || s.Host == h.Name {
			out = append(out, s)
		}
	}
	return out
}

// MatchHost reports whether a spec belongs on this host.
func (h *Host) MatchHost(s *appspec.Spec) bool {
	return s.Host == "" || s.Host == h.Name
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
