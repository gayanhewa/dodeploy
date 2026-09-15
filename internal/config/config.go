// Package config loads dodeploy's global configuration: the provider
// credentials, and the hosts apps can be deployed to.
//
// The file lives at ~/.config/dodeploy/config.yaml and is expected to be private.
// Anything in it can also come from the environment, and secrets are resolved
// lazily so a simple `dodeploy status` does not require a DigitalOcean token.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration file.
type Config struct {
	Providers Providers       `yaml:"providers"`
	Hosts     map[string]Host `yaml:"hosts"`
	// AppPaths are directories to scan for apps, one level deep:
	// "<path>/<project>/deploy/app.yaml".
	AppPaths []string `yaml:"app_paths"`
	path     string   // where it was loaded from, for error messages
}

// Providers holds credentials per provider.
type Providers struct {
	DigitalOcean DigitalOcean `yaml:"digitalocean"`
	Porkbun      Porkbun      `yaml:"porkbun"`
}

// DigitalOcean credentials. Token may be left empty, in which case it is read
// from DIGITALOCEAN_TOKEN and then from doctl's own config, so an existing
// doctl setup keeps working without duplicating the credential.
type DigitalOcean struct {
	Token string `yaml:"token"`
}

// Porkbun credentials for DNS.
type Porkbun struct {
	APIKey    string `yaml:"api_key"`
	SecretKey string `yaml:"secret_key"`
}

// Host is a machine that apps are deployed to.
type Host struct {
	Region     string `yaml:"region"`
	Size       string `yaml:"size"`
	Image      string `yaml:"image"`
	ReservedIP bool   `yaml:"reserved_ip"`
	Monitoring bool   `yaml:"monitoring"`

	// Names, ids or fingerprints of keys authorised on the droplet. Empty means
	// every key on the account.
	SSHKeys []string `yaml:"ssh_keys"`
	// Private key used for this tool's own ssh and rsync connections.
	SSHIdentity string `yaml:"ssh_identity"`

	CaddyEmail string `yaml:"caddy_email"`
	// Default marks the host used when an app's spec does not name one.
	Default bool `yaml:"default"`
}

// DefaultConfigPath is where the configuration is read from.
func DefaultConfigPath() string {
	if p := os.Getenv("DODEPLOY_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "dodeploy", "config.yaml")
}

// Load reads the configuration and applies environment overrides.
func Load() (*Config, error) {
	path := DefaultConfigPath()

	cfg := &Config{Hosts: map[string]Host{}}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(body, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cfg.path = path
	case errors.Is(err, os.ErrNotExist):
		cfg.path = path // defaults only; providers may still come from the env
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if cfg.Hosts == nil {
		cfg.Hosts = map[string]Host{}
	}
	return cfg, nil
}

// Path is where the configuration was read from.
func (c *Config) Path() string { return c.path }

// HostNames lists configured hosts, sorted.
func (c *Config) HostNames() []string {
	names := make([]string, 0, len(c.Hosts))
	for name := range c.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Host returns a host by name. An empty name selects the default host, or the
// only host when exactly one is configured.
func (c *Config) Host(name string) (string, Host, error) {
	if name != "" {
		h, ok := c.Hosts[name]
		if !ok {
			return "", Host{}, fmt.Errorf("no host named %q in %s (have: %s)",
				name, c.path, strings.Join(c.HostNames(), ", "))
		}
		return name, h, nil
	}

	for n, h := range c.Hosts {
		if h.Default {
			return n, h, nil
		}
	}
	if len(c.Hosts) == 1 {
		for n, h := range c.Hosts {
			return n, h, nil
		}
	}
	if len(c.Hosts) == 0 {
		return "", Host{}, fmt.Errorf("no hosts configured in %s", c.path)
	}
	return "", Host{}, fmt.Errorf("several hosts configured and none marked default: %s",
		strings.Join(c.HostNames(), ", "))
}

// DOToken resolves the DigitalOcean token: configuration first, then the
// environment, then doctl's own config file.
func (c *Config) DOToken() (string, error) {
	if t := strings.TrimSpace(c.Providers.DigitalOcean.Token); t != "" {
		return t, nil
	}
	for _, key := range []string{"DIGITALOCEAN_TOKEN", "DIGITALOCEAN_ACCESS_TOKEN", "DO_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(key)); t != "" {
			return t, nil
		}
	}
	if t := tokenFromDoctl(); t != "" {
		return t, nil
	}
	return "", errors.New("no DigitalOcean token: set providers.digitalocean.token, " +
		"export DIGITALOCEAN_TOKEN, or run `doctl auth init`")
}

// tokenFromDoctl reads the token doctl already holds, so an existing setup works
// without copying the credential anywhere.
//
// doctl follows the platform convention rather than XDG: ~/.config/doctl on
// Linux, ~/Library/Application Support/doctl on macOS. Both are checked.
func tokenFromDoctl() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	var paths []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		paths = append(paths, filepath.Join(x, "doctl", "config.yaml"))
	}
	paths = append(paths,
		filepath.Join(home, ".config", "doctl", "config.yaml"),
		filepath.Join(home, "Library", "Application Support", "doctl", "config.yaml"),
		filepath.Join(home, ".doctlcfg"),
	)

	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var parsed struct {
			AccessToken string `yaml:"access-token"`
		}
		if err := yaml.Unmarshal(body, &parsed); err != nil {
			continue
		}
		if t := strings.TrimSpace(parsed.AccessToken); t != "" {
			return t
		}
	}
	return ""
}

// PorkbunCreds resolves DNS credentials from configuration or environment.
func (c *Config) PorkbunCreds() (apiKey, secret string, err error) {
	apiKey = strings.TrimSpace(c.Providers.Porkbun.APIKey)
	secret = strings.TrimSpace(c.Providers.Porkbun.SecretKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("PORKBUN_API_KEY"))
	}
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("PORKBUN_SECRET_KEY"))
	}
	if apiKey == "" || secret == "" {
		return "", "", errors.New("no Porkbun credentials: set providers.porkbun in the " +
			"config, or export PORKBUN_API_KEY and PORKBUN_SECRET_KEY")
	}
	return apiKey, secret, nil
}
