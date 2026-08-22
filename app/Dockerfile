# syntax=docker/dockerfile:1

# Pinned to a specific patch release so builds are reproducible.
# For stricter supply-chain guarantees pin by digest: node:20.19.4-alpine3.21@sha256:...
ARG NODE_VERSION=20.19.4-alpine3.21

# ---------- Stage 1: production dependencies only ----------
FROM node:${NODE_VERSION} AS deps
WORKDIR /app
COPY package.json package-lock.json ./
# npm ci installs exactly what the lockfile says — no surprise upgrades in CI.
RUN npm ci --omit=dev && npm cache clean --force

# ---------- Stage 2: runtime ----------
FROM node:${NODE_VERSION} AS runtime

LABEL org.opencontainers.image.title="k8s-lab-app" \
      org.opencontainers.image.description="Sample service demonstrating production Kubernetes practices" \
      org.opencontainers.image.licenses="MIT"

ENV NODE_ENV=production \
    NPM_CONFIG_UPDATE_NOTIFIER=false

WORKDIR /app

# node:alpine ships an unprivileged "node" user (uid 1000). Never run as root.
COPY --chown=node:node --from=deps /app/node_modules ./node_modules
COPY --chown=node:node package.json ./
COPY --chown=node:node src ./src

USER node

EXPOSE 3000

# Container-level check. Kubernetes uses its own probes; this helps plain
# `docker run` and local compose setups.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:3000/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

# Exec form (no shell): the process becomes PID 1 and receives SIGTERM directly.
CMD ["node", "src/index.js"]
