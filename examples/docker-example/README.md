# docker-example

A hello world page served by Node.js in an Alpine container. It is the
container counterpart of a systemd binary app: same loopback port, same Caddy,
same `/healthz`.

```
server.js        the app: Node HTTP server, port and interface from the env
package.json     manifest (no dependencies)
Dockerfile       node:22-alpine, runs as the non-root `node` user
deploy/app.yaml  the dodeploy spec
```

## Run it with plain docker

The example works on its own, without dodeploy:

```bash
docker build -t docker-example examples/docker-example
docker run --rm -p 127.0.0.1:3001:8080 docker-example
curl http://127.0.0.1:3001/healthz
open http://127.0.0.1:3001        # the "Hello from Docker" page
```

Two details in that command are the whole contract:

- `-p 127.0.0.1:3001:8080` publishes the container's `8080` to the host's
  **loopback** port `3001`. Binding `0.0.0.0` instead would bypass the host
  firewall (docker writes its own iptables rules) and break the "adding an app
  is never a firewall change" property. Caddy then proxies to `127.0.0.1:3001`
  exactly as it does for a systemd app.
- The process listens on `0.0.0.0` inside the container, not `127.0.0.1`,
  because docker's published port forwards to the container's interface rather
  than its own loopback. dodeploy writes this into the app's `.env` for a
  container app automatically.

## Deploy it with dodeploy

The build happens on the host, so nothing is pushed to a registry. The host
needs docker, which the base image installs; a server that already runs
containers (for example, one running Minecraft) is ready as-is.

```bash
# 1. Point the name somewhere. Caddy uses its own CA for .test, so the browser
#    warns on first visit; that is expected for a local test.
echo "<host-ip>  dodeploy.test" | sudo tee -a /etc/hosts

# 2. Deploy. The path form is used because the example is not under an
#    app_path from the config.
dodeploy deploy examples/docker-example

# 3. Inspect.
dodeploy status
dodeploy logs docker-example
open https://dodeploy.test
```

The pieces, and why each is what it is:

| Spec | Value | Why |
|---|---|---|
| `port` | `3001` | The host's loopback port. Caddy knows nothing else. |
| `docker.container_port` | `8080` | What the process listens on inside the container. |
| `tls` | `internal` | `.test` is not publicly resolvable, so ACME cannot issue for it. |
| `runtime` | `docker` | Build with `docker build` and run with `docker run` instead of `go build`. |

On the host, a container app is still a systemd unit, so `status`, `logs`,
health checks and restarts behave exactly as they do for a binary app. The unit
runs `docker run` in the foreground so container output lands in journald, and
it deliberately omits the systemd sandbox the binary unit uses: the docker
client needs the daemon socket, which is root-equivalent, and the isolation now
comes from the container instead.

`dodeploy deploy` with `--skip-build` reuses the existing image without
rebuilding it. Volumes are supported with `docker.volumes` entries of the form
`<host-subdir>:<absolute-container-path>`; the example needs none.
