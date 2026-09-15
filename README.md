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
```

Every command is idempotent. `provision` reuses an existing droplet and reserved
IP and reconciles DNS rather than duplicating it; `deploy` skips the proxy reload
when the generated configuration is unchanged.

## Design notes

Things that were learned the hard way, kept here so they are not relearned.

**Builds happen on the host.** A binary linking libsql needs cgo and its prebuilt
native libraries must match the target exactly. Cross-compiling from a laptop
fails in ways that are hard to diagnose: zig cannot resolve the Rust unwinder
symbols in `libsql_experimental.a`, and nix's cross toolchain emits a binary whose
ELF interpreter points into the nix store and which wants a newer glibc than the
target ships.

**DNS reconciliation removes shadowing records.** A new domain at a registrar
usually ships an `ALIAS` on the apex and a wildcard `CNAME` pointing at parking.
Either will keep answering instead of the droplet while everything appears
correctly configured, so `EnsureA` deletes both. NS records are never touched.

**Reserved IPs are preferred over the anchor address.** A droplet with a reserved
IP attached has two public IPv4s and the API lists the anchor first. The anchor is
also the address that changes if the droplet is rebuilt, which is the entire
point of reserving one.

**The cloud-init template is checked for non-ASCII bytes** before a droplet is
created. cloud-init discards its whole configuration on encountering one, and
does so silently: the host boots with nothing installed and no error logged. A
single em-dash in a comment cost a full provisioning cycle.

**Proxy configuration is validated before reload.** An invalid Caddyfile would
otherwise take every app on the host down at once.

**State lives outside the synced source.** `deploy` uses `rsync --delete`, so
anything under the application's source directory is destroyed on the next
deploy. Databases and uploads belong in `data/`.

**The `.env` file is written once and never again.** Secrets are added there by
hand; overwriting it on each deploy would silently destroy them.

## Tests

```bash
go test ./...
```

The DNS and proxy tests cover the parts with real subtlety: parking records that
shadow a new A record, the fully-qualified/short name asymmetry between the
registrar's read and write APIs, idempotent updates, and proxy output that is
stable regardless of input order so reloads can be skipped.
