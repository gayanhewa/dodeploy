package host

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gayanhewa/dodeploy/internal/appspec"
	"github.com/gayanhewa/dodeploy/internal/cloudflare"
	"github.com/gayanhewa/dodeploy/internal/envfile"
)

// Cloudflare returns an API client, resolving credentials on first use.
//
// Like DNS, this is only called by a command that needs it, so an app that does
// not opt in never requires the credential to exist.
func (h *Host) Cloudflare() (*cloudflare.Client, error) {
	token, account, err := h.cfg.CloudflareCreds()
	if err != nil {
		return nil, err
	}
	return cloudflare.New(token, account), nil
}

// TurnstileOptions controls a Turnstile reconcile.
type TurnstileOptions struct {
	// Check reports drift without writing anything.
	Check bool
	// RotateSecret issues a fresh secret and writes it to the app.
	RotateSecret bool
	// Prune makes the widget's domain set exact instead of additive.
	Prune bool
}

// TurnstileReport is what one reconcile did (or would do).
type TurnstileReport struct {
	Widget      cloudflare.Result
	EnvChanged  bool
	Restarted   bool
	EnvPath     string
	SiteKeyName string
}

// ConfigureTurnstile reconciles an app's Turnstile widget and writes the key
// pair into its .env.
//
// The widget itself is created once by name and updated in place, so its secret
// is never disturbed by a routine run. The .env is edited rather than replaced,
// keeping the write-once invariant: only the two managed keys change, and the
// service is restarted only when a value actually moved.
func (h *Host) ConfigureTurnstile(ctx context.Context, spec *appspec.Spec, opts TurnstileOptions) (TurnstileReport, error) {
	var report TurnstileReport
	if !spec.TurnstileEnabled() {
		return report, fmt.Errorf("app %q has no cloudflare.turnstile block in its spec", spec.Name)
	}

	client, err := h.Cloudflare()
	if err != nil {
		return report, err
	}

	want := cloudflare.Widget{
		Name:    spec.TurnstileWidgetName(),
		Mode:    spec.TurnstileMode(),
		Domains: spec.TurnstileDomains(),
	}

	res, err := client.EnsureWidget(ctx, want, cloudflare.EnsureOptions{DryRun: opts.Check, Prune: opts.Prune})
	if err != nil {
		return report, err
	}
	report.Widget = res

	// Rotation writes to the account, so a check never does it.
	if opts.RotateSecret && !opts.Check {
		rotated, err := client.RotateSecret(ctx, res.Widget.SiteKey, false)
		if err != nil {
			return report, err
		}
		if rotated.Secret == "" {
			rotated.Secret = res.Widget.Secret
		}
		res.Widget = *rotated
		report.Widget = res
	}

	// A widget that is about to be created has no key pair to compare yet, so
	// there is nothing to check beyond the fact that it is missing.
	if opts.Check && res.Created {
		return report, cloudflare.ErrDrift
	}
	if res.Widget.Secret == "" && !opts.Check {
		return report, fmt.Errorf("cloudflare returned no secret for widget %q", want.Name)
	}

	siteKeyName, secretKeyName := spec.TurnstileEnvNames()
	path := h.AppDir(spec) + "/.env"
	report.EnvPath = path
	report.SiteKeyName = siteKeyName

	if !h.Remote.RunTolerant(ctx, "sudo test -f "+quote(path)) {
		return report, fmt.Errorf("%s does not exist yet: deploy once with `dodeploy deploy %s` before adding Turnstile",
			path, spec.Name)
	}

	content, err := h.Remote.Output(ctx, "sudo cat "+quote(path))
	if err != nil {
		return report, err
	}

	kv := map[string]string{siteKeyName: res.Widget.SiteKey}
	if res.Widget.Secret != "" {
		kv[secretKeyName] = res.Widget.Secret
	}

	updated, changed := envfile.Set(content, kv)
	report.EnvChanged = changed

	if opts.Check {
		if res.Changed() || changed {
			return report, cloudflare.ErrDrift
		}
		return report, nil
	}

	if !changed {
		return report, nil
	}

	mode, owner := h.fileModeAndOwner(ctx, path)
	if err := h.Remote.WriteFile(ctx, path, updated, mode, owner); err != nil {
		return report, err
	}

	// systemd reads the environment file at start, so a changed value only
	// takes effect after a restart.
	if _, err := h.Remote.Run(ctx, "sudo systemctl restart "+spec.ServiceName()); err != nil {
		return report, err
	}
	report.Restarted = true
	return report, nil
}

// fileModeAndOwner reads a file's mode and owner so it can be rewritten without
// changing either. The .env is hand-managed, so its permissions are the
// operator's choice, not this tool's.
func (h *Host) fileModeAndOwner(ctx context.Context, path string) (os.FileMode, string) {
	mode := os.FileMode(0o640)
	owner := "root:" + ServiceUser

	if out, err := h.Remote.Output(ctx, "stat -c %a "+quote(path)); err == nil {
		if parsed, err := strconv.ParseUint(strings.TrimSpace(out), 8, 32); err == nil {
			mode = os.FileMode(parsed)
		}
	}
	if out, err := h.Remote.Output(ctx, "stat -c %U:%G "+quote(path)); err == nil {
		if v := strings.TrimSpace(out); v != "" {
			owner = v
		}
	}
	return mode, owner
}
