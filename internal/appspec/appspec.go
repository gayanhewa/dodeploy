// Package appspec reads the file an application uses to describe how it is
// deployed.
//
// The spec lives with the application, at deploy/app.yaml, so a repository
// carries its own deployment requirements rather than a central list having to
// be kept in step with it.
package appspec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the spec file, relative to an application's root.
const FileName = "deploy/app.yaml"

// Spec describes one deployable application.
type Spec struct {
	// Root is the application directory, not part of the file.
	Root string `yaml:"-"`

	Name string `yaml:"name"`
	// Host names the machine in the tool's configuration. Empty uses the default.
	Host string `yaml:"host"`
	// Domain is the canonical hostname served.
	Domain string `yaml:"domain"`
	// Aliases are extra hostnames redirected to Domain, e.g. www.
	Aliases []string `yaml:"aliases"`
	// Port is the loopback port the app listens on. Must be unique per host.
	Port int `yaml:"port"`
	// Health is the path used to check the app came up, e.g. /healthz.
	Health string `yaml:"health"`
	// Runtime is how the app is packaged: RuntimeBinary (the default) or
	// RuntimeDocker. Empty means binary.
	Runtime string `yaml:"runtime"`
	// TLS selects the certificate source. Empty lets Caddy use ACME, which
	// needs a publicly resolvable name. "internal" uses Caddy's own CA, which
	// is what a name behind a local hosts entry needs.
	TLS string `yaml:"tls"`

	Build  Build  `yaml:"build"`
	Docker Docker `yaml:"docker"`
	Static string `yaml:"static"`
	Data   string `yaml:"data"`

	// Env holds non-secret defaults written into the app's .env on first deploy.
	// The file is never overwritten afterwards, so edits survive.
	Env map[string]string `yaml:"env"`
}

// Runtime values. The zero value is treated as RuntimeBinary.
const (
	RuntimeBinary = "binary"
	RuntimeDocker = "docker"
)

// Build describes how the binary is produced.
type Build struct {
	// Package is the Go package to build, e.g. ./cmd/server.
	Package string `yaml:"package"`
	// Binary is the produced file name.
	Binary string `yaml:"binary"`
	// CGO enables cgo, which anything linking libsql needs.
	CGO bool `yaml:"cgo"`
	// Prebuild is a local command run before syncing, e.g. "make css".
	Prebuild string `yaml:"prebuild"`
	// Tags are extra Go build tags.
	Tags string `yaml:"tags"`
}

// Docker describes how a container app is built and run.
//
// The build happens on the host, so an image never has to be pushed to a
// registry: the synced source is the build context and the Dockerfile owns the
// toolchain, which is also why cgo is no longer a reason to refuse to
// cross-compile.
type Docker struct {
	// Context is the build context, relative to the application root.
	Context string `yaml:"context"`
	// File is the Dockerfile, relative to Context.
	File string `yaml:"file"`
	// ContainerPort is the port the process listens on inside the container.
	// Docker publishes the loopback Port above to it.
	ContainerPort int `yaml:"container_port"`
	// Target selects a stage in a multi-stage Dockerfile.
	Target string `yaml:"target"`
	// BuildArgs are passed through as --build-arg.
	BuildArgs map[string]string `yaml:"build_args"`
	// Volumes are bind mounts as "<host-subdir>:<absolute-container-path>",
	// where the host subdirectory is relative to the app's directory on the
	// host. It is created if missing.
	Volumes []string `yaml:"volumes"`
}

// Discover finds app specs one level under each root: "<root>/<project>/deploy/app.yaml".
//
// Scanning rather than keeping a central registry means adding an app is a
// matter of adding a spec to its repository, with nothing else to keep in step.
func Discover(roots []string) ([]*Spec, error) {
	var out []*Spec
	for _, root := range roots {
		root = expandHome(root)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // a missing search path is not fatal
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
				continue
			}
			spec, err := Load(dir)
			if err != nil {
				return nil, err
			}
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Find returns the app with the given name from a set of specs.
func Find(specs []*Spec, name string) (*Spec, error) {
	for _, s := range specs {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return nil, fmt.Errorf("no app named %q (found: %s)", name, strings.Join(names, ", "))
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads the spec for the application rooted at dir.
func Load(dir string) (*Spec, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(abs, FileName)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no app spec at %s\n"+
			"      create one with: dodeploy new <name> <domain> --dir %s", path, abs)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var s Spec
	if err := yaml.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Root = abs

	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func (s *Spec) validate() error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case !namePattern.MatchString(s.Name):
		return fmt.Errorf("name %q must be lower-case letters, digits and dashes", s.Name)
	case s.Domain == "":
		return errors.New("domain is required")
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("port %d must be between 1 and 65535", s.Port)
	}

	if s.Health == "" {
		s.Health = "/healthz"
	}
	if !strings.HasPrefix(s.Health, "/") {
		return fmt.Errorf("health %q must start with /", s.Health)
	}

	switch s.TLS {
	case "", "internal":
	default:
		return fmt.Errorf("tls %q must be empty or \"internal\"", s.TLS)
	}

	switch s.Runtime {
	case "", RuntimeBinary:
		s.Runtime = RuntimeBinary
		if s.Build.Package == "" {
			return errors.New("build.package is required, e.g. ./cmd/server")
		}
		if s.Build.Binary == "" {
			return errors.New("build.binary is required, e.g. server")
		}
	case RuntimeDocker:
		if s.Docker.Context == "" {
			s.Docker.Context = "."
		}
		if s.Docker.File == "" {
			s.Docker.File = "Dockerfile"
		}
		if s.Docker.ContainerPort < 1 || s.Docker.ContainerPort > 65535 {
			return fmt.Errorf("docker.container_port %d must be between 1 and 65535", s.Docker.ContainerPort)
		}
		for i, v := range s.Docker.Volumes {
			hostPath, containerPath, ok := strings.Cut(v, ":")
			if !ok || strings.TrimSpace(hostPath) == "" || strings.TrimSpace(containerPath) == "" {
				return fmt.Errorf("docker.volumes[%d] %q must be <host-subdir>:<container-path>", i, v)
			}
			if filepath.IsAbs(hostPath) || strings.Contains(hostPath, "..") {
				return fmt.Errorf("docker.volumes[%d] host path %q must be a subdirectory of the app", i, hostPath)
			}
			if !strings.HasPrefix(containerPath, "/") {
				return fmt.Errorf("docker.volumes[%d] container path %q must be absolute", i, containerPath)
			}
		}
	default:
		return fmt.Errorf("runtime %q must be %q or %q", s.Runtime, RuntimeBinary, RuntimeDocker)
	}
	return nil
}

// Domains returns the canonical domain followed by any aliases.
func (s *Spec) Domains() []string {
	out := []string{s.Domain}
	for _, a := range s.Aliases {
		if a != "" && a != s.Domain {
			out = append(out, a)
		}
	}
	return out
}

// RemoteDir is where the application lives on the host.
func (s *Spec) RemoteDir(remoteRoot string) string {
	return filepath.Join(remoteRoot, s.Name)
}

// ServiceName is the systemd unit name.
func (s *Spec) ServiceName() string { return s.Name }

// IsDocker reports whether the app is packaged as a container.
func (s *Spec) IsDocker() bool { return s.Runtime == RuntimeDocker }

// DockerImage is the local tag the image is built and run as. It is never
// pushed anywhere: the build happens on the host.
func (s *Spec) DockerImage() string { return s.Name + ":latest" }

// ContainerName is the name of the running container.
func (s *Spec) ContainerName() string { return s.Name }

// EnvFile renders the initial .env for a first deploy.
//
// Values are written with a comment explaining that the file is managed by hand,
// because it is created once and never overwritten: secrets added later must not
// be lost by a redeploy.
func (s *Spec) EnvFile(baseURL string) string {
	var b strings.Builder
	b.WriteString("# Created by dodeploy on first deploy. This file is never overwritten,\n")
	b.WriteString("# so edits and secrets added here survive redeploys.\n\n")

	// A container must bind its own interface, not its loopback: docker's
	// published port forwards to the container's address, so a process on the
	// container's 127.0.0.1 would be unreachable from the host.
	host, port := "127.0.0.1", s.Port
	if s.IsDocker() {
		host, port = "0.0.0.0", s.Docker.ContainerPort
	}

	writeEnv(&b, "APP_ENV", "prod")
	writeEnv(&b, "APP_BASE_URL", baseURL)
	writeEnv(&b, "HOST", host)
	writeEnv(&b, "PORT", fmt.Sprint(port))
	writeEnv(&b, "LOG_LEVEL", "info")
	b.WriteString("\n")

	for _, k := range sortedKeys(s.Env) {
		writeEnv(&b, k, s.Env[k])
	}
	return b.String()
}

func writeEnv(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "%s=%s\n", key, value)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
