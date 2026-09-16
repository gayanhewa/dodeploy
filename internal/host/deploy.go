package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gayanhewa/dodeploy/internal/appspec"
	"github.com/gayanhewa/dodeploy/internal/caddy"
	"github.com/gayanhewa/dodeploy/internal/systemd"
)

// DeployOptions adjusts a deploy.
type DeployOptions struct {
	// SkipBuild installs the binary already on the host.
	SkipBuild bool
	// SkipProxy leaves the reverse proxy config alone.
	SkipProxy bool
	// Timeout bounds the health check.
	Timeout time.Duration
}

// Deploy builds and installs one app, then routes it.
//
// The build happens on the host. A binary linking libsql needs cgo, its
// prebuilt native libraries have to match the target exactly, and cross
// compiling from a laptop fails in ways that are hard to diagnose: zig cannot
// resolve the Rust unwinder symbols, and nix's cross toolchain emits a binary
// hard-linked to its own libc. Building where it runs avoids all of it.
func (h *Host) Deploy(ctx context.Context, spec *appspec.Spec, all []*appspec.Spec, opts DeployOptions) error {
	if opts.Timeout == 0 {
		opts.Timeout = 90 * time.Second
	}

	remoteDir := h.AppDir(spec)
	if err := h.checkSSH(ctx); err != nil {
		return err
	}

	h.Logf("deploying %s to %s:%s", spec.Name, h.Remote.Address, remoteDir)

	if err := h.prebuild(ctx, spec); err != nil {
		return err
	}
	if err := h.ensureRemoteDirs(ctx, spec, remoteDir); err != nil {
		return err
	}
	if err := h.syncSource(ctx, spec, remoteDir); err != nil {
		return err
	}
	if err := h.ensureEnvFile(ctx, spec, remoteDir); err != nil {
		return err
	}
	if !opts.SkipBuild {
		switch {
		case spec.IsDocker():
			if err := h.buildImage(ctx, spec, remoteDir); err != nil {
				return err
			}
		default:
			if err := h.buildOnHost(ctx, spec, remoteDir); err != nil {
				return err
			}
		}
	}
	if spec.IsDocker() {
		if err := h.installContainer(ctx, spec, remoteDir); err != nil {
			return err
		}
	} else {
		if err := h.installBinary(ctx, spec, remoteDir); err != nil {
			return err
		}
		if err := h.syncStatic(ctx, spec, remoteDir); err != nil {
			return err
		}
		if err := h.installUnit(ctx, spec, remoteDir); err != nil {
			return err
		}
	}
	if !opts.SkipProxy {
		if err := h.applyProxy(ctx, all); err != nil {
			return err
		}
	}
	if err := h.healthCheck(ctx, spec, opts.Timeout); err != nil {
		return err
	}

	fmt.Fprintln(h.Out)
	h.Logf("%s is live at https://%s", spec.Name, spec.Domain)
	h.Logf("logs: ssh %s 'sudo journalctl -u %s -f'", h.Remote.Target(), spec.ServiceName())
	return nil
}

// prebuild runs a local build step, typically a stylesheet compile that needs
// tooling we would rather not install on a 512MB host.
func (h *Host) prebuild(ctx context.Context, spec *appspec.Spec) error {
	if strings.TrimSpace(spec.Build.Prebuild) == "" {
		return nil
	}
	h.Logf("prebuild: %s", spec.Build.Prebuild)

	cmd := exec.CommandContext(ctx, "sh", "-c", spec.Build.Prebuild)
	cmd.Dir = spec.Root
	cmd.Stdout = h.Out
	cmd.Stderr = h.Out
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prebuild %q: %w", spec.Build.Prebuild, err)
	}
	return nil
}

// ensureRemoteDirs creates the app directory owned by the deployer, so rsync can
// write into it, while bin/ and data/ are handed to the service user afterwards.
func (h *Host) ensureRemoteDirs(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	cmd := fmt.Sprintf(
		"sudo mkdir -p %s/src %s/bin %s/data %s/static && sudo chown -R %s %s",
		quote(remoteDir), quote(remoteDir), quote(remoteDir), quote(remoteDir),
		quote(h.Remote.User), quote(remoteDir))
	if _, err := h.Remote.Run(ctx, cmd); err != nil {
		return err
	}
	return nil
}

// syncSource mirrors the repository to the host, deleting files that no longer
// exist locally. Anything under this directory is therefore disposable, which is
// why state lives in data/ outside it.
func (h *Host) syncSource(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	h.Logf("syncing source")

	excludes := []string{
		".git", "node_modules", "bin", "data", ".env",
		"*.db", "*.db-wal", "*.db-shm",
	}
	return h.Remote.Sync(ctx, spec.Root, remoteDir+"/src/", excludes)
}

// ensureEnvFile writes the app's environment once, and never again.
//
// Secrets are added here by hand after the first deploy, so overwriting the file
// on every deploy would silently destroy them.
func (h *Host) ensureEnvFile(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	path := remoteDir + "/.env"
	if h.Remote.RunTolerant(ctx, "test -f "+quote(path)) {
		return nil
	}

	h.Logf("creating .env (edit it to add secrets; it is never overwritten)")
	base := "https://" + spec.Domain
	if err := h.Remote.WriteFile(ctx, path, spec.EnvFile(base), 0o640, "root:"+ServiceUser); err != nil {
		return err
	}
	return nil
}

func (h *Host) buildOnHost(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	h.Logf("building on host (cgo is why this is not a cross-compile)")

	cgo := "0"
	if spec.Build.CGO {
		cgo = "1"
	}

	cmd := fmt.Sprintf(
		"cd %s/src && PATH=$PATH:/usr/local/go/bin CGO_ENABLED=%s GOFLAGS=-mod=mod go build -tags %s -o %s/src/bin/%s %s",
		quote(remoteDir), cgo, quote(spec.Build.Tags),
		quote(remoteDir), spec.Build.Binary, spec.Build.Package)

	out, err := h.Remote.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		fmt.Fprintln(h.Out, out)
	}
	return nil
}

func (h *Host) installBinary(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	cmd := fmt.Sprintf("sudo install -m 0755 -o %s -g %s %s/src/bin/%s %s/bin/%s",
		ServiceUser, ServiceUser, quote(remoteDir), spec.Build.Binary, quote(remoteDir), spec.Build.Binary)
	_, err := h.Remote.Run(ctx, cmd)
	return err
}

func (h *Host) syncStatic(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	if spec.Static == "" {
		return nil
	}
	local := filepath.Join(spec.Root, spec.Static)
	if _, err := os.Stat(local); err != nil {
		return nil // nothing to sync is not an error
	}

	h.Logf("syncing %s", spec.Static)
	if err := h.Remote.Sync(ctx, local, remoteDir+"/src/"+spec.Static+"/", []string{"uploads"}); err != nil {
		return err
	}

	// Place it and hand it to the service user, which is what the unit's
	// ReadWritePaths expects.
	cmd := fmt.Sprintf(
		"sudo rsync -a --delete %s/src/%s/ %s/%s/ && sudo chown -R %s:%s %s/%s",
		quote(remoteDir), spec.Static, quote(remoteDir), spec.Static,
		ServiceUser, ServiceUser, quote(remoteDir), spec.Static)
	_, err := h.Remote.Run(ctx, cmd)
	return err
}

// installUnit writes and starts the unit that runs the app's binary.
func (h *Host) installUnit(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	unit := systemd.Render(systemd.Unit{
		Name:             spec.ServiceName(),
		User:             ServiceUser,
		WorkingDirectory: remoteDir,
		EnvFile:          remoteDir + "/.env",
		ExecStart:        remoteDir + "/bin/" + spec.Build.Binary,
		ReadWritePaths:   []string{remoteDir + "/data", remoteDir + "/static"},
	})

	unitPath := "/etc/systemd/system/" + spec.ServiceName() + ".service"
	if err := h.Remote.WriteFile(ctx, unitPath, unit, 0o644, "root:root"); err != nil {
		return err
	}

	// The app must own its data directory before it starts.
	cmd := fmt.Sprintf("sudo chown -R %s:%s %s/data %s/bin", ServiceUser, ServiceUser, quote(remoteDir), quote(remoteDir))
	if _, err := h.Remote.Run(ctx, cmd); err != nil {
		return err
	}

	cmd = fmt.Sprintf("sudo systemctl daemon-reload && sudo systemctl enable %s >/dev/null 2>&1; sudo systemctl restart %s",
		spec.ServiceName(), spec.ServiceName())
	_, err := h.Remote.Run(ctx, cmd)
	return err
}

// buildImage builds the app's container image on the host.
//
// The build happens where it runs for the same reason a cgo binary is built
// there: the base image defines the toolchain, so there is nothing to
// cross-compile. The synced source is the build context, so no image ever has
// to be pushed to a registry.
func (h *Host) buildImage(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	if !h.Remote.RunTolerant(ctx, "sudo docker info >/dev/null 2>&1") {
		return fmt.Errorf("cannot reach the docker daemon on %s; a container app needs docker installed and running", h.Name)
	}

	contextDir := filepath.Join(remoteDir, "src", spec.Docker.Context)
	dockerfile := filepath.Join(contextDir, spec.Docker.File)

	args := []string{
		"sudo", "docker", "build",
		"--file", quote(dockerfile),
		"--tag", quote(spec.DockerImage()),
		"--progress", "plain",
	}
	for _, k := range sortedKeys(spec.Docker.BuildArgs) {
		args = append(args, "--build-arg", quote(k+"="+spec.Docker.BuildArgs[k]))
	}
	if spec.Docker.Target != "" {
		args = append(args, "--target", quote(spec.Docker.Target))
	}
	args = append(args, quote(contextDir))

	h.Logf("building image %s", spec.DockerImage())
	out, err := h.Remote.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		fmt.Fprintln(h.Out, out)
	}
	return nil
}

// installContainer writes and starts the unit that runs the app's container.
//
// systemd remains the supervisor, so status, logs and restart behaviour are the
// same as for a binary app. The container is run in the foreground: systemd
// then tracks it directly, and the container's output reaches journald, which
// is what `dodeploy logs` reads.
func (h *Host) installContainer(ctx context.Context, spec *appspec.Spec, remoteDir string) error {
	unit := systemd.Render(systemd.Unit{
		Name: spec.ServiceName(),
		Docker: &systemd.DockerRun{
			Image:   spec.DockerImage(),
			Name:    spec.ContainerName(),
			Publish: fmt.Sprintf("127.0.0.1:%d:%d", spec.Port, spec.Docker.ContainerPort),
			EnvFile: remoteDir + "/.env",
			// The .env may predate the app becoming a container, so the two
			// values a container cannot take from the file are set explicitly.
			Env: []string{
				"HOST=0.0.0.0",
				fmt.Sprintf("PORT=%d", spec.Docker.ContainerPort),
			},
			Volumes: h.containerVolumes(spec, remoteDir),
		},
	})

	unitPath := "/etc/systemd/system/" + spec.ServiceName() + ".service"
	if err := h.Remote.WriteFile(ctx, unitPath, unit, 0o644, "root:root"); err != nil {
		return err
	}

	cmd := fmt.Sprintf("sudo systemctl daemon-reload && sudo systemctl enable %s >/dev/null 2>&1; sudo systemctl restart %s",
		spec.ServiceName(), spec.ServiceName())
	_, err := h.Remote.Run(ctx, cmd)
	return err
}

// containerVolumes turns the spec's host-subdir:container-path pairs into bind
// mounts, anchoring each host path inside the app's directory on the host.
func (h *Host) containerVolumes(spec *appspec.Spec, remoteDir string) []string {
	out := make([]string, 0, len(spec.Docker.Volumes))
	for _, v := range spec.Docker.Volumes {
		hostPath, containerPath, ok := strings.Cut(v, ":")
		if !ok {
			continue // already rejected by spec validation
		}
		out = append(out, filepath.Join(remoteDir, hostPath)+":"+containerPath)
	}
	return out
}

// sortedKeys returns a map's keys in a stable order, so a generated command is
// identical between runs.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// applyProxy regenerates the shared proxy config from every app on this host.
//
// It is rendered from all of them rather than just the one being deployed, so
// the file always reflects the whole host. It is only written and reloaded when
// the content actually changed, and validated before the reload: an invalid
// Caddyfile would otherwise take every app down at once.
func (h *Host) applyProxy(ctx context.Context, all []*appspec.Spec) error {
	apps := h.sharedApps(all)
	if err := checkPorts(apps); err != nil {
		return err
	}
	if err := checkDomains(apps); err != nil {
		return err
	}

	desired := caddy.Render(h.Config.CaddyEmail, sites(apps))

	current, err := h.Remote.Run(ctx, "sudo cat /etc/caddy/Caddyfile 2>/dev/null || true")
	if err != nil {
		return err
	}
	if strings.TrimSpace(current) == strings.TrimSpace(desired) {
		h.Logf("proxy config unchanged")
		return h.ensureCaddyRunning(ctx)
	}

	h.Logf("updating proxy config for %d app(s)", len(apps))
	if err := h.Remote.WriteFile(ctx, "/etc/caddy/Caddyfile", desired, 0o644, "root:root"); err != nil {
		return err
	}

	if !h.Remote.RunTolerant(ctx, "sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1") {
		return errors.New("the generated Caddyfile is invalid; the running proxy was left untouched")
	}

	// reload-or-restart also starts the proxy if it is not running, which is the
	// state a host is in before anything has been deployed to it.
	cmd := "sudo systemctl enable caddy >/dev/null 2>&1; sudo systemctl reload-or-restart caddy"
	if _, err := h.Remote.Run(ctx, cmd); err != nil {
		return err
	}
	h.Logf("proxy reloaded")
	return nil
}

func (h *Host) ensureCaddyRunning(ctx context.Context) error {
	if h.Remote.RunTolerant(ctx, "systemctl is-active --quiet caddy") {
		return nil
	}
	_, err := h.Remote.Run(ctx, "sudo systemctl enable --now caddy")
	return err
}

// healthCheck polls the app on its loopback port, so a failure is the app's and
// not DNS or TLS.
func (h *Host) healthCheck(ctx context.Context, spec *appspec.Spec, timeout time.Duration) error {
	h.Logf("waiting for %s to answer on %s", spec.Name, spec.Health)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := fmt.Sprintf("curl -fsS --max-time 5 %s", quote(fmt.Sprintf("http://127.0.0.1:%d%s", spec.Port, spec.Health)))
		if h.Remote.RunTolerant(ctx, cmd) {
			h.Logf("health check passed")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// Show why rather than just that.
	logs, _ := h.Remote.Run(ctx, fmt.Sprintf("sudo journalctl -u %s -n 40 --no-pager", spec.ServiceName()))
	return fmt.Errorf("%s did not become healthy within %s\n\n%s", spec.Name, timeout, indent(logs))
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
