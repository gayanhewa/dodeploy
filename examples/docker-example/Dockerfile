# Alpine with Node.js on it, as the user asked.
FROM node:22-alpine

WORKDIR /app

# Copy the manifest on its own first. Docker caches each layer, so editing
# server.js does not re-run the dependency install and the next build is quick.
COPY package.json ./
RUN npm install --omit=dev

COPY server.js ./

# Not root. For a container app the container is the security boundary, so the
# process inside should not run as root any more than a systemd unit should.
USER node

# The port the app listens on inside the container. The host publishes to this
# over loopback; it is deliberately not the same as the host's public port.
EXPOSE 8080

# Useful for `docker run` on its own. dodeploy checks health from the host over
# the loopback port instead, so this is not what makes the deploy pass.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s \
  CMD wget -qO- "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

CMD ["node", "server.js"]
