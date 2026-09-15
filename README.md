# dodeploy

Provision DigitalOcean droplets and deploy applications to them.

One host runs one Caddy reverse proxy on ports 80 and 443. Each app is a systemd
service on a loopback port, so **adding an app is a configuration change, never a
firewall change**.

```
                     ┌──────────────────────────────────────────┐
   :80 :443  ───────▶│  Caddy  (TLS, HTTP→HTTPS, alias→apex)    │
                     └───────┬───────────────┬──────────────────┘
                             │               │
                  127.0.0.1:3001     127.0.0.1:3002      … 3001-3099 reserved
                             │               │
                     ┌───────▼──────┐ ┌──────▼───────┐
                     │ app: one     │ │ app: two     │   each a systemd unit
                     │ user: apps   │ │ user: apps   │
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
`deploy/app.yaml`:

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
```

The spec lives with the app rather than in a central registry, so nothing has to
be kept in step.

## Commands

```bash
dodeploy new my-app my-app.com --dir ~/Workspace/my-app   # write a spec
dodeploy apps                                             # list discovered apps
dodeploy provision                                        # create the host + DNS
dodeploy deploy ~/Workspace/my-app                        # build and install
dodeploy deploy --all                                     # everything found
dodeploy status                                           # hosts and health
dodeploy logs my-app                                      # follow the journal
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
