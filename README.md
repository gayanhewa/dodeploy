# dodeploy

Provision DigitalOcean droplets and deploy applications to them.

One host runs one Caddy reverse proxy on ports 80 and 443. Each app is a systemd
service on a loopback port, so **adding an app is a configuration change, never a
firewall change**.

An app is either a **compiled binary** (the default) or a **container**. Caddy
only ever talks to `127.0.0.1:<port>`, so it does not know or care which:

```
                     ┌──────────────────────────────────────────┐
   :80 :443  ───────▶│  Caddy  (TLS, HTTP→HTTPS, alias→apex)    │
                     └───────┬───────────────┬──────────────────┘
                             │               │
                  127.0.0.1:3001     127.0.0.1:3002      … 3001-3099 reserved
                             │               │
                     ┌───────▼──────┐ ┌──────▼───────┐
                     │ app: binary  │ │ app: docker  │  each a systemd unit
                     │ user: apps   │ │ container    │
                     └──────────────┘ └──────────────┘
```

## Install

```bash
go build -o ~/bin/dodeploy ./cmd/dodeploy
```

Requires `ssh` and `rsync` locally. The DigitalOcean token is read from the
config, then `$DIGITALOCEAN_TOKEN`, then **doctl's own config**, so an existing
doctl setup needs no extra credential.

## Configure

`~/.config/dodeploy/config.yaml` (see `examples/config.yaml`):

```yaml
providers:
  digitalocean:
    token: ""            # empty: falls back to doctl
  porkbun:
    api_key: ""
    secret_key: ""
  cloudflare:            # only needed for apps that opt into Turnstile
    api_token: ""        # empty: falls back to $CLOUDFLARE_API_TOKEN
    account_id: ""       # empty: falls back to $CLOUDFLARE_ACCOUNT_ID

# Scanned one level deep for <path>/<project>/deploy/app.yaml
app_paths:
  - ~/Workspace

hosts:
  apps-prod:
    region: syd1
    size: s-1vcpu-512mb-10gb
    image: ubuntu-24-04-x64
    reserved_ip: true
    ssh_keys: []                 # empty: every key on the account
    ssh_identity: ~/.ssh/dodeploy
    caddy_email: admin@example.com
    default: true
```

## Per-app spec

Each application declares how it is built and served, in its own repository, at
`deploy/app.yaml`. The spec lives with the app rather than in a central registry,
so nothing has to be kept in step.

The top half is the same everywhere. `runtime` then says how the app is packaged:

| `runtime` | Built with | Runs as |
|---|---|---|
| *(empty)* or `binary` | `go build` on the host | a systemd unit, as user `apps` |
| `docker` | `docker build` on the host | a container, supervised by a systemd unit |

### Binary app (the default)

```yaml
name: my-app
host: apps-prod          # optional; the default host otherwise
domain: my-app.com
port: 3001               # loopback, unique per host
health: /healthz

build:
  package: ./cmd/server
  binary: server
  cgo: true
  prebuild: make css     # run locally before syncing

static: static
data: data

env:                     # non-secret defaults, written once
  LOG_LEVEL: info

cloudflare:              # optional; omit and dodeploy never calls Cloudflare
  turnstile:
    mode: managed        # managed (default), non-interactive, or invisible
    widget: ""           # name a shared widget to reuse it across apps
    domains: [localhost] # extra hostnames beyond domain + aliases
```

### Container app

`runtime: docker` builds the synced source on the host and runs the image as a
container. Nothing is pushed to a registry: the Dockerfile owns the toolchain, so
the build happens where it runs. A container app has no `build:` block; instead
`docker.container_port` says which port the process listens on inside the
container, while `port` stays the host's loopback port. A host created by
`dodeploy provision` already has docker installed.

```yaml
name: my-app
host: apps-prod
domain: my-app.com
port: 3001               # loopback, unique per host
health: /healthz

runtime: docker
docker:
  context: .             # build context, relative to the app root
  file: Dockerfile       # Dockerfile, relative to the context
  container_port: 8080   # what the process listens on inside the container
  # target: build        # optional multi-stage target
  # build_args:          # optional --build-arg, e.g. values baked into a bundle
  #   NEXT_PUBLIC_APP_URL: https://my-app.com
  # volumes:             # optional bind mounts, <host-subdir>:<container-path>
  #   - data:/data

env:                     # non-secret defaults, written once
  LOG_LEVEL: info
```

Two things a container app has to get right, and dodeploy handles both:

- It publishes on the host's **loopback** port, never `0.0.0.0`. Docker writes
  its own iptables rules, so a public bind would bypass the firewall; dodeploy
  always renders `127.0.0.1:<port>:<container_port>`.
- Its process listens on `0.0.0.0` **inside** the container, because the
  published port forwards to the container's interface rather than its loopback.
  For this reason dodeploy writes `HOST=0.0.0.0` and `PORT=<container_port>`
  into a container app's `.env` instead of the binary defaults.

A working container app lives in
[`examples/docker-example`](examples/docker-example).

### TLS

Caddy obtains a certificate with ACME, which needs a publicly resolvable name.
For one that cannot have it - `localhost`, or a `*.test` name behind a local
hosts entry - set `tls: internal` and Caddy signs it with its own CA instead:

```yaml
domain: my-app.test
tls: internal
```

### Cloudflare (Turnstile)

Cloudflare is opt-in per app: add a `cloudflare.turnstile` block and dodeploy can
keep a Turnstile widget in step with the app, then write the key pair into its
`.env`. Apps without the block are never touched, and the credential is only
asked for when a command actually needs it.

```bash
dodeploy cloudflare turnstile my-app          # reconcile the widget and keys
dodeploy cloudflare turnstile --all           # every app that opts in
dodeploy cloudflare turnstile my-app --check  # report drift, change nothing
dodeploy cloudflare widgets                   # list widgets in the account
```

The widget is named after the app unless `widget:` names another, so several apps
can share one. Domains are additive by default, so reconciling one app never
drops a hostname another relies on; `--prune` makes the set exact.
`--rotate-secret` issues a fresh secret.

The site key and secret are written to `TURNSTILE_SITE_KEY` and
`TURNSTILE_SECRET_KEY` unless `env:` renames them. Only those lines are changed:
the `.env` is otherwise left exactly as it is, and the service restarts only when
a value actually moved. Deploy the app once first, so the `.env` exists.
`--check` exits **2** when anything is out of date, so it can gate a deploy.

## Monitoring

`dodeploy health` takes a snapshot of a host: CPU, load, memory, swap, disk and
the state of every app on it. It reads `/proc` and `df` over ssh, so a droplet
needs no agent installed.

```bash
dodeploy health                  # one snapshot
dodeploy health --app my-app     # one app
dodeploy health --json           # for a dashboard or log
dodeploy health --watch 5s       # refresh until interrupted
```

Anything over a threshold is flagged. The thresholds are percentages and default
to 90, and can be changed or disabled (0):

```bash
dodeploy health --cpu 80 --mem 85 --disk 90
```

The exit code makes it usable from a monitor: **0** healthy, **2** degraded (a
threshold crossed or an app unhealthy), **1** the check itself failed.

## Commands

```bash
dodeploy new my-app my-app.com --dir ~/Workspace/my-app   # write a spec
dodeploy apps                                             # list discovered apps
dodeploy provision                                        # create the host + DNS
dodeploy deploy ~/Workspace/my-app                        # build and install
dodeploy deploy --all                                     # everything found
dodeploy status                                           # hosts and health
dodeploy health --watch 5s                                # cpu, memory, disk, apps
dodeploy logs my-app                                      # follow the journal
dodeploy cloudflare turnstile my-app                      # Turnstile widget + keys
dodeploy ssh                                              # shell on the host
dodeploy skills install --global                          # teach your AI agent
```

Every command is idempotent. `provision` reuses an existing droplet and reserved
IP and reconciles DNS rather than duplicating it; `deploy` skips the proxy reload
when the generated configuration is unchanged.

## Agent skill

The `dodeploy` skill teaches an AI coding assistant the config format, the app
spec, and the commands above. It is embedded in the binary, so one executable is
enough to install it.

```bash
dodeploy skills install              # this project: .agents/skills + .claude/skills
dodeploy skills install --global     # every project: ~/.agents/skills + ~/.claude/skills
dodeploy skills install --dir PATH   # an explicit skills directory (repeatable)
dodeploy skills list                 # where it is installed
dodeploy skills show                 # print SKILL.md
```

An existing, identical installation is left alone; one with different content is
only overwritten with `--force`.

## Tests

```bash
go test ./...
```

The DNS and proxy tests cover the parts with real subtlety: parking records that
shadow a new A record, the fully-qualified/short name asymmetry between the
registrar's read and write APIs, idempotent updates, and proxy output that is
stable regardless of input order so reloads can be skipped.

## License

MIT — see [LICENSE](LICENSE).
