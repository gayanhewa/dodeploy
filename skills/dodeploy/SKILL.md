---
name: dodeploy
description: Provision DigitalOcean droplets and deploy applications to them with the `dodeploy` CLI. Use when creating, deploying, resizing, or debugging a dodeploy host or app, writing or editing a deploy/app.yaml spec, adding a new app or domain, reading dodeploy logs or status, or when a task mentions dodeploy, DigitalOcean provisioning, reserved IPs, Caddy reverse proxying, or Porkbun DNS for this tool.
---

# dodeploy

`dodeploy` provisions DigitalOcean droplets and deploys applications to them.
One host runs a single Caddy reverse proxy on ports 80 and 443; each app is a
systemd service on a loopback port. **Adding an app is a configuration change,
never a firewall change.**

## When to use this skill

Use it whenever the user works with the `dodeploy` CLI or a `deploy/app.yaml`
spec: standing up a host, shipping an app, adding a domain, resizing a droplet,
or diagnosing a failed deploy. The tool is deliberately small; almost every task
is one of the commands below plus an edit to a spec.

## Mental model

```
   :80 :443  ───────▶  Caddy (TLS, HTTP→HTTPS, alias→apex)
                          │ 127.0.0.1:3001, :3002, … (3001-3099 reserved)
                          ▼
                       one systemd unit per app, user "apps", /srv/apps/<name>
```

- The spec lives with the app, at `deploy/app.yaml`, not in a central registry.
- Ports are loopback only and must be unique per host. The tool picks the next
  free one for you.
- DNS is reconciled, not recreated: `provision` reuses an existing droplet and
  reserved IP.
- `deploy` skips the proxy reload when the generated Caddy config is unchanged.
- The app's `.env` on the host is written **once** and never overwritten, so
  secrets added by hand survive redeploys.

## Prerequisites

- The `dodeploy` binary on `PATH` (`go build -o ~/bin/dodeploy ./cmd/dodeploy`).
- Local `ssh` and `rsync`.
- A DigitalOcean token, resolved from, in order: `providers.digitalocean.token`,
  `$DIGITALOCEAN_TOKEN` (or `$DIGITALOCEAN_ACCESS_TOKEN`, `$DO_TOKEN`), then
  doctl's own config. An existing doctl setup needs no extra credential.
- Porkbun credentials for DNS, or `--no-dns`.

Check what a machine has before changing anything:

```bash
dodeploy version
dodeploy apps          # apps discovered in the configured search paths
dodeploy status        # hosts and app health
```

## Configuration

Global config lives at `~/.config/dodeploy/config.yaml` (override with
`$DODEPLOY_CONFIG`). It holds credentials, so keep it `chmod 600`. See
`examples/config.yaml` in the repository.

```yaml
providers:
  digitalocean:
    token: ""            # empty: falls back to $DIGITALOCEAN_TOKEN, then doctl
  porkbun:
    api_key: ""          # empty: falls back to $PORKBUN_API_KEY
    secret_key: ""       # empty: falls back to $PORKBUN_SECRET_KEY
  cloudflare:            # only needed for apps that opt into Turnstile
    api_token: ""        # empty: falls back to $CLOUDFLARE_API_TOKEN
    account_id: ""       # empty: falls back to $CLOUDFLARE_ACCOUNT_ID

# Scanned one level deep for <path>/<project>/deploy/app.yaml
app_paths:
  - ~/Workspace

hosts:
  apps-prod:             # the key is the droplet name
    region: syd1
    size: s-1vcpu-512mb-10gb
    image: ubuntu-24-04-x64
    reserved_ip: true    # survives destroy/recreate; keeps DNS stable
    monitoring: true
    ssh_keys: []         # names, ids or fingerprints; empty = every key on the account
    ssh_identity: ~/.ssh/dodeploy
    caddy_email: admin@example.com
    default: true
```

Key points:

- `app_paths` are scanned one level deep only. An app is any directory
  `<path>/<project>/` containing `deploy/app.yaml`.
- `ssh_identity` must be set if the private key is not one of ssh's defaults,
  otherwise every connection fails with `Permission denied (publickey)`.
- A host with `default: true` is used by apps whose spec omits `host`. With a
  single host configured, it is used automatically.

## App spec (`deploy/app.yaml`)

```yaml
name: my-app
host: apps-prod          # optional; the default host otherwise
domain: my-app.com
aliases: [www.my-app.com]
port: 3001               # loopback, unique per host
health: /healthz

build:
  package: ./cmd/server  # Go package to build
  binary: server
  cgo: true              # needed by anything linking libsql
  prebuild: make css     # run locally before syncing
  tags: ""               # extra Go build tags

static: static           # synced to the host as-is, e.g. CSS/JS/assets
data: data

env:                     # non-secret defaults, written on first deploy only
  LOG_LEVEL: info

cloudflare:              # optional; omit and dodeploy never calls Cloudflare
  turnstile:
    mode: managed        # managed (default), non-interactive, or invisible
    widget: ""           # name a shared widget to reuse it across apps
    domains: [localhost] # extra hostnames beyond domain + aliases
    env:                 # names the key pair is written to
      site_key: TURNSTILE_SITE_KEY
      secret_key: TURNSTILE_SECRET_KEY
```

Rules the loader enforces:

- `name` matches `^[a-z0-9][a-z0-9-]*$`, and must be unique in the search paths.
- `domain`, `build.package` and `build.binary` are required.
- `port` must be 1-65535 and unique per host.
- `health` defaults to `/healthz` and must start with `/`.

Write a spec with `dodeploy new` rather than by hand when starting fresh; it
assigns the next free port and fills in the host.

## Commands

```bash
# create a host, wait for boot, point DNS at it
dodeploy provision [--host NAME] [--dns-only] [--no-dns] [--recreate] [--timeout 20m]

# build and install an app, then route it
dodeploy deploy <path> | --name NAME | --all   [--skip-build] [--skip-proxy]

# inspect
dodeploy status [--host NAME]
dodeploy health [--host NAME] [--app NAME] [--watch 5s] [--json]
dodeploy apps
dodeploy logs <name> [--host NAME] [--lines N] [--follow]

# write a new spec
dodeploy new <name> <domain> [--dir DIR] [--host NAME] [--port N]
                             [--package PKG] [--binary BIN]

# capacity
dodeploy sizes  [--host NAME]
dodeploy resize [--host NAME] [--size SLUG] [--disk] [--snapshot] [--yes]

# DNS records
#   show: list a domain's records
#   txt:  add a TXT record, e.g. a domain-verification token
#         (adds without replacing, so SPF/DKIM on the same name survive)
dodeploy dns show <domain>
dodeploy dns txt  <name> <value>

# Cloudflare (opt-in per app; apps without a cloudflare.turnstile block are
# never touched, and credentials are only needed by these commands)
#   reconcile the widget named after the app, then write its key pair to .env
#   --check exits 2 when anything is out of date, so it can gate a deploy
#   --prune makes the domain set exact; --rotate-secret issues a fresh secret
#   widgets lists every widget in the account (never their secrets)
dodeploy cloudflare turnstile <app> | --all  [--check] [--rotate-secret] [--prune]
dodeploy cloudflare widgets

# shell and agent skill
dodeploy ssh   [--host NAME]
dodeploy skills install [--global] [--dir DIR] [--force]
dodeploy skills list
dodeploy skills show
```

All commands are idempotent. `--recreate` is destructive and prompts for the
host name before destroying the droplet.

TXT records are added alongside whatever is already on the name rather than
replacing it, because SPF, DKIM and a verification token commonly share one apex.
For example:

```bash
dodeploy dns txt example.com "google-site-verification=..."
```

## Standard workflows

### Deploy a brand-new app

```bash
dodeploy new my-app my-app.com --dir ~/Workspace/my-app
# edit ~/Workspace/my-app/deploy/app.yaml if needed
dodeploy provision                 # only if the host does not exist yet
dodeploy deploy ~/Workspace/my-app
```

### Redeploy an existing app

```bash
dodeploy deploy --name my-app
```

### Ship every discovered app

```bash
dodeploy deploy --all
```

### Add another app to an existing host

`dodeploy new` assigns the next free port and the default host, so a second app
needs no host changes at all:

```bash
dodeploy new second-app second-app.com --dir ~/Workspace/second-app
dodeploy deploy ~/Workspace/second-app
```

### Configure Turnstile for an app

Add a `cloudflare.turnstile` block to the app's spec, deploy it once so its
`.env` exists, then reconcile:

```bash
dodeploy deploy ~/Workspace/my-app
dodeploy cloudflare turnstile my-app
```

The command treats the widget as declarative: it creates one named after the app
if it is missing, adds the app's domain and aliases, and writes
`TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_KEY` into the app's `.env`, changing only
those lines and restarting only when a value moved. Several apps share one widget
by naming the same `widget:`. Re-running is a no-op; `--check` reports drift
without writing and exits 2, so it can gate CI.

### Monitor a host

```bash
dodeploy health                      # one snapshot: cpu, memory, disk, apps
dodeploy health --app my-app         # just one app
dodeploy health --json               # machine readable, for a dashboard
dodeploy health --watch 5s           # refresh until interrupted
```

`health` reads `/proc` and `df` on the host, so a bare droplet needs no agent
installed. It flags anything above a threshold and any unhealthy app. Three
thresholds default to 90% and can be changed or disabled with `--cpu`, `--mem`
and `--disk` (0 disables a check).

Exit codes make it usable from a monitor or cron: **0** healthy, **2** degraded
(a threshold crossed or an app unhealthy), **1** the check itself failed.

```bash
# cron: alert when the box is degraded, and say why
dodeploy health --json > /tmp/health.json || alert < /tmp/health.json
```

Use `status` when you want to know what is deployed, and `health` when you want
to know how the machine and its apps are doing right now.

### Diagnose a failed deploy

```bash
dodeploy status                    # is the host up, is the app healthy?
dodeploy logs my-app               # journal for the systemd unit
dodeploy ssh                       # then: systemctl status my-app
```

If only DNS is wrong, reconcile it without touching the droplet:

```bash
dodeploy provision --dns-only
```

### Grow a host

```bash
dodeploy sizes                     # see slugs, memory, vcpu, disk, price
dodeploy resize --size s-2vcpu-2gb --disk
```

`--disk` permanently grows the disk and cannot be undone. A snapshot is taken
first by default (`--snapshot=false` to skip); snapshots are billed monthly until
deleted. The droplet is powered off during the resize.

## Troubleshooting

| Symptom | Likely cause and fix |
| --- | --- |
| `no app spec at .../deploy/app.yaml` | Wrong directory, or spec not named exactly `deploy/app.yaml`. Create with `dodeploy new`. |
| `no app named "x"` | The app is outside `app_paths`, or deeper than one level. Check `dodeploy apps`. |
| `no host named "x"` | `host:` in the spec does not match a key under `hosts:`. Check `dodeploy status`. |
| `Permission denied (publickey)` | `ssh_identity` is unset or wrong, or the key is not among `ssh_keys` on the account. |
| App unhealthy after deploy | The service did not come up; check `dodeploy logs`. Confirm the app listens on `$PORT`/`127.0.0.1` and serves `health`. |
| High disk/cpu/memory reported | Confirm with `dodeploy health`; grow the disk with `dodeploy resize --disk` (permanent) or move data off the host. |
| Port already in use | Two apps share a port on one host. Reassign one (`dodeploy new` picks a free port). |
| TLS never issues | DNS for the domain does not resolve to the host. Run `dodeploy provision --dns-only`, or `--no-dns` if managing DNS elsewhere. |
| `no DigitalOcean token` | Set `providers.digitalocean.token`, export `$DIGITALOCEAN_TOKEN`, or run `doctl auth init`. |
| `no Porkbun credentials` | Set `providers.porkbun`, export `$PORKBUN_API_KEY`/`$PORKBUN_SECRET_KEY`, or pass `--no-dns`. |
| Config edits ignored | `$DODEPLOY_CONFIG` points somewhere other than `~/.config/dodeploy/config.yaml`. |
| `no Cloudflare credentials` | Set `providers.cloudflare` in the config, or export `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`. |
| Cloudflare 403 | The token needs **Account → Turnstile → Edit**; create a scoped token rather than using a Global API Key. |
| `... does not exist yet` from `cloudflare turnstile` | Deploy the app once first: the key pair is written into its `.env`, which only exists after a deploy. |

## Gotchas

- Ports 3001-3099 are reserved for loopback apps; do not expose them publicly.
- The `.env` on the host is created only on first deploy. Change `env:` in the
  spec and it will **not** propagate to an existing app; edit the file on the
  host instead.
- `deploy` builds on the host, so the droplet needs enough memory for the build.
  Use `--skip-build` when the binary is already present.
- App discovery is one level deep. Nested monorepo layouts need each app to have
  its own top-level directory under an `app_paths` entry.
- A reserved IP keeps DNS stable across droplet recreation; without one, DNS
  follows the anchor address and changes on rebuild.
