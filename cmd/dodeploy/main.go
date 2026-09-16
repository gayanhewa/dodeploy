// Command dodeploy provisions DigitalOcean droplets and deploys applications to
// them.
//
// One host runs one Caddy reverse proxy on ports 80 and 443; each app is a
// systemd service on a loopback port. Apps declare how they are built and served
// in deploy/app.yaml inside their own repository, so this tool carries no
// knowledge of any particular application.
//
//	dodeploy provision [--host NAME] [--dns-only] [--recreate]
//	dodeploy deploy    <path|--name NAME|--all> [--skip-build] [--skip-proxy]
//	dodeploy status    [--host NAME] [--all]
//	dodeploy apps
//	dodeploy new       <name> <domain> [--dir DIR] [--host NAME] [--port N]
//	dodeploy ssh       [--host NAME]
//	dodeploy logs      <name> [--lines N]
//	dodeploy health    [--host NAME] [--app NAME] [--watch 5s] [--json]
//	dodeploy skills    install [--global] [--dir DIR] [--force]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gayanhewa/dodeploy/internal/appspec"
	"github.com/gayanhewa/dodeploy/internal/config"
	"github.com/gayanhewa/dodeploy/internal/dns"
	"github.com/gayanhewa/dodeploy/internal/host"
	"github.com/gayanhewa/dodeploy/internal/skill"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error

	switch cmd {
	case "provision":
		err = cmdProvision(ctx, args)
	case "deploy":
		err = cmdDeploy(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "health":
		err = cmdHealth(ctx, args)
	case "sizes":
		err = cmdSizes(ctx, args)
	case "resize":
		err = cmdResize(ctx, args)
	case "dns":
		err = cmdDNS(ctx, args)
	case "apps":
		err = cmdApps(args)
	case "new":
		err = cmdNew(args)
	case "ssh":
		err = cmdSSH(ctx, args)
	case "logs":
		err = cmdLogs(ctx, args)
	case "skills":
		err = cmdSkills(args)
	case "version", "-v", "--version":
		fmt.Println("dodeploy", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		// Degraded is a result, not a failure: the report has already been
		// printed, so exit 2 lets a monitor alert without parsing it.
		if errors.Is(err, host.ErrDegraded) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `dodeploy %s - provision DigitalOcean hosts and deploy apps to them

Usage:
  dodeploy <command> [flags]

Commands:
  provision   Create a host, wait for it to be ready, point DNS at it
  deploy      Build and install an app, then route it
  status      Show a host and the apps on it
  health      Sample cpu, memory, disk and app health on a host
  sizes       List droplet sizes available in a host's region
  resize      Grow a host's droplet (powers it off; a disk resize is permanent)
  dns         Show a domain's records, or add a TXT record
  apps        List apps found in the configured search paths
  new         Write a deploy/app.yaml for a new app
  ssh         Open a shell on a host
  logs        Follow an app's logs
  skills      Install the agent skill that documents this tool

Run "dodeploy <command> -h" for the flags of a command.

Configuration:
  %s

`, version, config.DefaultConfigPath())
}

// ---------------------------------------------------------------------------
// provision
// ---------------------------------------------------------------------------

func cmdProvision(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("provision", flag.ExitOnError)
	var (
		hostName = fs.String("host", "", "host to provision (defaults to the configured default)")
		dnsOnly  = fs.Bool("dns-only", false, "only reconcile DNS records")
		noDNS    = fs.Bool("no-dns", false, "leave DNS alone")
		recreate = fs.Bool("recreate", false, "destroy the droplet first (destructive)")
		timeout  = fs.Duration("timeout", 20*time.Minute, "how long to wait for first-boot")
	)
	fs.Parse(args)

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}

	// DNS only needs the host's address, not a droplet that is ready.
	if *dnsOnly {
		if err := h.Resolve(ctx); err != nil {
			return err
		}
		return h.Provision(ctx, specs, host.ProvisionOptions{SkipDNS: *noDNS, Timeout: *timeout})
	}

	if *recreate {
		fmt.Fprintf(os.Stdout, "This will DESTROY the droplet %q and everything on it.\n", h.Name)
		fmt.Fprint(os.Stdout, "Type the host name to confirm: ")
		var typed string
		fmt.Scanln(&typed)
		if strings.TrimSpace(typed) != h.Name {
			return fmt.Errorf("aborted")
		}
	}

	return h.Provision(ctx, specs, host.ProvisionOptions{
		SkipDNS:  *noDNS,
		Timeout:  *timeout,
		Recreate: *recreate,
	})
}

// ---------------------------------------------------------------------------
// deploy
// ---------------------------------------------------------------------------

func cmdDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	var (
		byName    = fs.String("name", "", "deploy an app by name from the search paths")
		all       = fs.Bool("all", false, "deploy every app in the search paths")
		skipBuild = fs.Bool("skip-build", false, "install the binary already on the host")
		skipProxy = fs.Bool("skip-proxy", false, "leave the reverse proxy config alone")
	)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return fmt.Errorf("no apps found; add deploy/app.yaml to a project under %s",
			strings.Join(cfg.AppPaths, ", "))
	}

	var selected []*appspec.Spec
	switch {
	case *all:
		selected = specs
	case *byName != "":
		spec, err := appspec.Find(specs, *byName)
		if err != nil {
			return err
		}
		selected = []*appspec.Spec{spec}
	case len(positional) > 0:
		spec, err := appspec.Load(positional[0])
		if err != nil {
			return err
		}
		selected = []*appspec.Spec{spec}
	default:
		return fmt.Errorf("name an app directory, or use --name or --all")
	}

	for _, spec := range selected {
		h, err := host.New(cfg, spec.Host, os.Stdout)
		if err != nil {
			return err
		}
		if err := h.Resolve(ctx); err != nil {
			return err
		}
		if err := h.Deploy(ctx, spec, specs, host.DeployOptions{
			SkipBuild: *skipBuild,
			SkipProxy: *skipProxy,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// status / apps
// ---------------------------------------------------------------------------

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	hostName := fs.String("host", "", "host to inspect")
	fs.Parse(args)

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}
	return h.Status(ctx, specs)
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func cmdHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	var (
		hostName = fs.String("host", "", "host to sample")
		appName  = fs.String("app", "", "only report this app")
		asJSON   = fs.Bool("json", false, "emit the snapshot as JSON")
		watch    = fs.Duration("watch", 0, "sample repeatedly at this interval until interrupted")
		count    = fs.Int("count", 0, "stop after this many samples (0 means forever)")
		cpuMax   = fs.Float64("cpu", 90, "degrade above this CPU percentage (0 disables)")
		memMax   = fs.Float64("mem", 90, "degrade above this memory percentage (0 disables)")
		diskMax  = fs.Float64("disk", 90, "degrade above this disk percentage (0 disables)")
	)
	fs.Parse(args)

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	if *appName != "" {
		spec, err := appspec.Find(specs, *appName)
		if err != nil {
			return err
		}
		specs = []*appspec.Spec{spec}
	}

	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}
	if err := h.Resolve(ctx); err != nil {
		return err
	}

	opts := host.HealthOptions{CPUMax: *cpuMax, MemMax: *memMax, DiskMax: *diskMax}

	sample := func() (bool, error) {
		health, err := h.Health(ctx, specs, opts)
		if err != nil {
			return false, err
		}
		if *asJSON {
			encoded, err := json.MarshalIndent(health, "", "  ")
			if err != nil {
				return false, err
			}
			fmt.Println(string(encoded))
		} else {
			h.PrintHealth(health)
		}
		return health.Degraded(), nil
	}

	if *watch <= 0 {
		degraded, err := sample()
		if err != nil {
			return err
		}
		if degraded {
			return host.ErrDegraded
		}
		return nil
	}

	clear := isTerminal(os.Stdout)
	degraded := false
	for i := 0; *count == 0 || i < *count; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return degradedErr(degraded)
			case <-time.After(*watch):
			}
			if clear && !*asJSON {
				fmt.Fprint(os.Stdout, "\033[2J\033[H")
			}
		}
		degraded, err = sample()
		if err != nil {
			return err
		}
	}
	return degradedErr(degraded)
}

func degradedErr(degraded bool) error {
	if degraded {
		return host.ErrDegraded
	}
	return nil
}

// isTerminal reports whether f is a character device, so escape sequences are
// only written to something that will interpret them.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func cmdApps(args []string) error {
	fs := flag.NewFlagSet("apps", flag.ExitOnError)
	fs.Parse(args)

	_, specs, err := load()
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Println("no apps found")
		return nil
	}

	fmt.Printf("%-24s %-34s %-8s %-14s %s\n", "NAME", "DOMAIN", "PORT", "HOST", "PATH")
	for _, s := range specs {
		hostName := s.Host
		if hostName == "" {
			hostName = "(default)"
		}
		fmt.Printf("%-24s %-34s %-8d %-14s %s\n", s.Name, s.Domain, s.Port, hostName, s.Root)
	}
	return nil
}

// ---------------------------------------------------------------------------
// sizes / resize
// ---------------------------------------------------------------------------

func cmdSizes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sizes", flag.ExitOnError)
	hostName := fs.String("host", "", "host whose region to list sizes for")
	fs.Parse(args)

	cfg, _, err := load()
	if err != nil {
		return err
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}

	sizes, err := h.AvailableSizes(ctx)
	if err != nil {
		return err
	}

	current, err := h.CurrentSizeSlug(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("sizes available in %s:\n\n", h.RegionSlug())
	fmt.Printf("  %-24s %7s %6s %6s %10s  %s\n", "SLUG", "MEMORY", "VCPU", "DISK", "PRICE/MO", "")
	for _, s := range sizes {
		marker := ""
		switch {
		case s.Slug == current:
			marker = "<- current"
		case s.Disk > 0:
			marker = "grows the disk permanently"
		}
		fmt.Printf("  %-24s %5dMB %6d %5dGB %9.2f  %s\n", s.Slug, s.Memory, s.VCPUs, s.Disk, s.PriceMonthly, marker)
	}
	fmt.Printf("\n  resize with: dodeploy resize --size <slug> --disk\n")
	return nil
}

func cmdResize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resize", flag.ExitOnError)
	var (
		hostName = fs.String("host", "", "host to resize")
		size     = fs.String("size", "", "target size slug (see `dodeploy sizes`)")
		disk     = fs.Bool("disk", false, "also grow the disk; permanent, cannot be undone")
		snapshot = fs.Bool("snapshot", true, "capture a snapshot first (billed monthly until deleted)")
		yes      = fs.Bool("yes", false, "skip the confirmation prompt")
		timeout  = fs.Duration("timeout", 15*time.Minute, "how long to wait for each step")
	)
	fs.Parse(args)

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}

	return h.Resize(ctx, host.ResizeOptions{
		Size:      *size,
		Disk:      *disk,
		Snapshot:  *snapshot,
		AssumeYes: *yes,
		Timeout:   *timeout,
		Specs:     specs,
	})
}

// ---------------------------------------------------------------------------
// dns
// ---------------------------------------------------------------------------

func cmdDNS(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage:\n" +
			"  dodeploy dns show <domain>\n" +
			"  dodeploy dns txt <name> <value>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	apiKey, secret, err := cfg.PorkbunCreds()
	if err != nil {
		return err
	}
	provider := dns.NewPorkbun(apiKey, secret)

	switch args[0] {
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("usage: dodeploy dns show <domain>")
		}
		records, err := provider.Records(ctx, args[1])
		if err != nil {
			return err
		}
		dns.SortRecords(records)

		fmt.Printf("records for %s:\n\n", args[1])
		fmt.Printf("  %-8s %-38s %s\n", "TYPE", "NAME", "CONTENT")
		for _, r := range records {
			if r.Type == "NS" {
				continue // nameservers are noise for most lookups
			}
			content := r.Content
			if len(content) > 58 {
				content = content[:55] + "..."
			}
			fmt.Printf("  %-8s %-38s %s\n", r.Type, r.Name, content)
		}
		return nil

	case "txt":
		if len(args) < 3 {
			return fmt.Errorf("usage: dodeploy dns txt <name> <value>")
		}
		result, err := dns.EnsureTXT(ctx, provider, args[1], args[2], "600")
		if err != nil {
			return err // ErrNotOptedIn already reads as an instruction
		}
		fmt.Println(result)
		return nil

	default:
		return fmt.Errorf("unknown dns subcommand %q; use show or txt", args[0])
	}
}

// ---------------------------------------------------------------------------
// new
// ---------------------------------------------------------------------------

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	var (
		dir      = fs.String("dir", ".", "directory to write the spec into")
		hostName = fs.String("host", "", "host this app belongs to")
		port     = fs.Int("port", 0, "loopback port (asks the config for a free one)")
		pkg      = fs.String("package", "", "Go package to build (defaults to ./cmd/<name>)")
		binary   = fs.String("binary", "", "binary name (defaults to the app name)")
	)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return fmt.Errorf("usage: dodeploy new <name> <domain> [--dir DIR]")
	}
	name, domain := positional[0], positional[1]

	if *pkg == "" {
		*pkg = "./cmd/" + name
	}
	if *binary == "" {
		*binary = name
	}

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	if _, err := appspec.Find(specs, name); err == nil {
		return fmt.Errorf("an app named %q already exists in the search paths", name)
	}

	// Pick the next free port unless one was given, so two apps on the same host
	// cannot collide.
	if *port == 0 {
		highest := 3000
		for _, s := range specs {
			if s.Port > highest {
				highest = s.Port
			}
		}
		*port = highest + 1
	}
	if *hostName == "" {
		*hostName = defaultHostName(cfg)
	}

	path := filepath.Join(*dir, filepath.FromSlash(appspec.FileName))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}

	spec := fmt.Sprintf(`# Deployment spec for %s. Read by dodeploy.
#
# dodeploy deploy %s

name: %s
host: %s
domain: %s
port: %d
health: /healthz

build:
  package: %s
  binary: %s
  # cgo is needed by anything linking libsql.
  cgo: true
  # Run locally before syncing, e.g. "make css". Leave empty if not needed.
  prebuild: ""

static: static
data: data

# Non-secret defaults written to the app's .env on first deploy. That file is
# never overwritten, so secrets added there survive redeploys.
env:
  LOG_LEVEL: info
`, name, *dir, name, *hostName, domain, *port, *pkg, *binary)

	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n\n", path)
	fmt.Printf("Next:\n\n  dodeploy deploy %s\n\n", *dir)
	return nil
}

func defaultHostName(cfg *config.Config) string {
	name, _, err := cfg.Host("")
	if err != nil {
		return ""
	}
	return name
}

// ---------------------------------------------------------------------------
// ssh / logs
// ---------------------------------------------------------------------------

func cmdSSH(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ssh", flag.ExitOnError)
	hostName := fs.String("host", "", "host to connect to")
	fs.Parse(args)

	cfg, _, err := load()
	if err != nil {
		return err
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}
	if err := h.Resolve(ctx); err != nil {
		return err
	}

	sshArgs := []string{"-o", "StrictHostKeyChecking=accept-new"}
	if h.Config.SSHIdentity != "" {
		sshArgs = append(sshArgs, "-i", h.Config.SSHIdentity)
	}
	sshArgs = append(sshArgs, h.Remote.Target())

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	hostName := fs.String("host", "", "host the app runs on")
	lines := fs.Int("lines", 100, "how many lines to show")
	follow := fs.Bool("follow", true, "follow the log")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: dodeploy logs <app-name>")
	}

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	spec, err := appspec.Find(specs, positional[0])
	if err != nil {
		return err
	}
	if *hostName == "" {
		*hostName = spec.Host
	}
	h, err := host.New(cfg, *hostName, os.Stdout)
	if err != nil {
		return err
	}
	if err := h.Resolve(ctx); err != nil {
		return err
	}

	cmd := fmt.Sprintf("sudo journalctl -u %s -n %d", spec.ServiceName(), *lines)
	if *follow {
		cmd += " -f"
	}
	if _, err := h.Remote.Run(ctx, cmd); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// skills
// ---------------------------------------------------------------------------

func cmdSkills(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dodeploy skills <install|list|show>")
	}
	switch args[0] {
	case "install":
		return cmdSkillsInstall(args[1:])
	case "list", "ls":
		return cmdSkillsList()
	case "show", "cat":
		return cmdSkillsShow()
	default:
		return fmt.Errorf("unknown skills subcommand %q (want install, list or show)", args[0])
	}
}

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdSkillsInstall(args []string) error {
	fs := flag.NewFlagSet("skills install", flag.ExitOnError)
	var (
		global = fs.Bool("global", false, "install for the current user (~) instead of this project")
		force  = fs.Bool("force", false, "overwrite an existing installation with different content")
		dirs   stringList
	)
	fs.Var(&dirs, "dir", "skills directory to install into, e.g. ~/.agents/skills (repeatable)")
	fs.Parse(args)

	var targets []skill.Target
	if len(dirs) > 0 {
		targets = skill.TargetsIn(dirs)
	} else {
		t, err := skill.Targets(*global)
		if err != nil {
			return err
		}
		targets = t
	}

	results, err := skill.Install(targets, *force)
	for _, r := range results {
		fmt.Printf("%-8s %-52s %s\n", r.Target.Label, r.Target.Dir, r.Status)
	}
	if err != nil {
		return err
	}

	for _, r := range results {
		if r.Status == skill.Installed {
			fmt.Printf("\nThe skill is available to agents that read skills from those directories.\n")
			break
		}
	}
	return nil
}

func cmdSkillsList() error {
	for _, global := range []bool{false, true} {
		scope := "project"
		if global {
			scope = "global"
		}
		targets, err := skill.Targets(global)
		if err != nil {
			return err
		}
		fmt.Printf("%s:\n", scope)
		for _, t := range targets {
			state := "not installed"
			if _, err := os.Stat(filepath.Join(t.Dir, "SKILL.md")); err == nil {
				state = "installed"
			}
			fmt.Printf("  %-8s %-52s %s\n", t.Label, t.Dir, state)
		}
	}
	return nil
}

func cmdSkillsShow() error {
	body, err := skill.Body()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(body)
	return err
}

// ---------------------------------------------------------------------------

// parseInterspersed parses a command's arguments so flags may appear before or
// after the positional ones.
//
// Go's flag package stops at the first non-flag argument, so "deploy . --skip-build"
// would silently ignore --skip-build and rebuild anyway. Silently ignoring a flag
// is the worst failure mode for a CLI, so the arguments are reordered first.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)

		// --name=value carries its own value.
		if strings.Contains(arg, "=") {
			continue
		}
		// A flag that takes a value needs the next argument too, unless it is a
		// boolean, which never does.
		if f := fs.Lookup(strings.TrimLeft(arg, "-")); f != nil {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		}
	}

	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}

// load reads the configuration and discovers apps in the configured paths.
func load() (*config.Config, []*appspec.Spec, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	specs, err := appspec.Discover(cfg.AppPaths)
	if err != nil {
		return nil, nil, err
	}
	return cfg, specs, nil
}
