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
package main

import (
	"context"
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
	"github.com/gayanhewa/dodeploy/internal/host"
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
	case "sizes":
		err = cmdSizes(ctx, args)
	case "resize":
		err = cmdResize(ctx, args)
	case "apps":
		err = cmdApps(args)
	case "new":
		err = cmdNew(args)
	case "ssh":
		err = cmdSSH(ctx, args)
	case "logs":
		err = cmdLogs(ctx, args)
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
  sizes       List droplet sizes available in a host's region
  resize      Grow a host's droplet (powers it off; a disk resize is permanent)
  apps        List apps found in the configured search paths
  new         Write a deploy/app.yaml for a new app
  ssh         Open a shell on a host
  logs        Follow an app's logs

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
	fs.Parse(args)

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
	case fs.NArg() > 0:
		spec, err := appspec.Load(fs.Arg(0))
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
	fs.Parse(args)

	if fs.NArg() < 2 {
		return fmt.Errorf("usage: dodeploy new <name> <domain> [--dir DIR]")
	}
	name, domain := fs.Arg(0), fs.Arg(1)

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
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: dodeploy logs <app-name>")
	}

	cfg, specs, err := load()
	if err != nil {
		return err
	}
	spec, err := appspec.Find(specs, fs.Arg(0))
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
