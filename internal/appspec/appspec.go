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

	Build  Build  `yaml:"build"`
	Static string `yaml:"static"`
	Data   string `yaml:"data"`

	// Env holds non-secret defaults written into the app's .env on first deploy.
	// The file is never overwritten afterwards, so edits survive.
	Env map[string]string `yaml:"env"`
}

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
	case s.Build.Package == "":
		return errors.New("build.package is required, e.g. ./cmd/server")
	case s.Build.Binary == "":
		return errors.New("build.binary is required, e.g. server")
	}

	if s.Health == "" {
		s.Health = "/healthz"
	}
	if !strings.HasPrefix(s.Health, "/") {
		return fmt.Errorf("health %q must start with /", s.Health)
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

// EnvFile renders the initial .env for a first deploy.
//
// Values are written with a comment explaining that the file is managed by hand,
// because it is created once and never overwritten: secrets added later must not
// be lost by a redeploy.
func (s *Spec) EnvFile(baseURL string) string {
	var b strings.Builder
	b.WriteString("# Created by dodeploy on first deploy. This file is never overwritten,\n")
	b.WriteString("# so edits and secrets added here survive redeploys.\n\n")

	writeEnv(&b, "APP_ENV", "prod")
	writeEnv(&b, "APP_BASE_URL", baseURL)
	writeEnv(&b, "HOST", "127.0.0.1")
	writeEnv(&b, "PORT", fmt.Sprint(s.Port))
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
