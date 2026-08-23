# syntax=docker/dockerfile:1
ARG NODE_VERSION=20.20.2-alpine3.22
ARG NGINX_VERSION=1.29-alpine

# ---------- Stage 1: build the bundle ----------
FROM node:${NODE_VERSION} AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY index.html vite.config.js ./
COPY src ./src
RUN npm run build

# ---------- Stage 2: serve it ----------
# nginx-unprivileged runs as uid 101 and binds 8080, so no root and no
# NET_BIND_SERVICE capability are required.
FROM nginxinc/nginx-unprivileged:${NGINX_VERSION} AS runtime

LABEL org.opencontainers.image.title="k8s-lab-web" \
      org.opencontainers.image.description="Interface for the k8s-lab service" \
      org.opencontainers.image.licenses="MIT"

USER 0
COPY nginx.conf /etc/nginx/nginx.conf
COPY security-headers.conf /etc/nginx/security-headers.conf
COPY --from=build /src/dist /usr/share/nginx/html
# One layer: patch the base packages and hand the served files to the
# unprivileged user the image already ships.
RUN apk upgrade --no-cache && \
    chown -R 101:101 /usr/share/nginx/html
USER 101

EXPOSE 8080

# Exec form, so no shell is started for every check.
HEALTHCHECK --interval=30s --timeout=3s --start-period=3s --retries=3 \
  CMD ["sh", "-c", "wget -qO- http://127.0.0.1:8080/healthz | grep -q ok"]

CMD ["nginx", "-g", "daemon off;"]
