// A hello world HTTP server, used to demonstrate a container app on dodeploy.
//
// The port contract matters more than the page: the process listens on the
// port and interface it is told to, and answers GET /healthz. Everything else
// is decoration.
//
// Inside a container the process must bind 0.0.0.0, not 127.0.0.1: docker's
// published port forwards to the container's interface, so a server on the
// container's own loopback is unreachable from the host.
const http = require("http");

const port = Number(process.env.PORT || 8080);
const host = process.env.HOST || "0.0.0.0";

const escapeHTML = (s) =>
  String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[c]);

const page = (req) => `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Hello from Docker</title>
    <style>
      :root { color-scheme: light dark; }
      body {
        margin: 0;
        min-height: 100vh;
        display: grid;
        place-items: center;
        font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
        background: #0b1020;
        color: #e7ecf5;
      }
      main { text-align: center; padding: 2rem; }
      h1 { margin: 0 0 .5rem; font-size: clamp(2rem, 6vw, 3.5rem); }
      p { margin: .25rem 0; color: #9aa7bd; }
      code { color: #7dd3fc; }
    </style>
  </head>
  <body>
    <main>
      <h1>Hello from Docker</h1>
      <p>Node.js on Alpine, served through Caddy.</p>
      <p><code>${escapeHTML(req.headers.host || "localhost")}</code></p>
    </main>
  </body>
</html>
`;

const server = http.createServer((req, res) => {
  if (req.url === "/healthz") {
    res.writeHead(200, { "content-type": "text/plain; charset=utf-8" });
    res.end("ok\n");
    return;
  }

  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(page(req));
});

server.listen(port, host, () => {
  console.log(`listening on http://${host}:${port}`);
});

// Docker sends SIGTERM on stop; shutting down cleanly lets the connection
// close instead of being cut off.
for (const signal of ["SIGTERM", "SIGINT"]) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
