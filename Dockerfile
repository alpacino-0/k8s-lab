# The control plane: the panel, the API and the deploy write path.
#
# Its own file rather than a stage in Dockerfile.operator, because the two are
# different programs with different lifetimes: the operator reconciles objects
# and the control plane serves people, and an install can reasonably run one
# without the other.
#
# This file did not exist until 2026-09-01, and its absence was the product's
# largest structural gap: `chart/` deploys the reference tenant in `app/`, the
# published `ghcr.io/damgahq/damga` image is built from `./app`, and
# docs/CONTROL-PLANE.md tells the reader to run the binary on a laptop. So the
# platform had a panel nobody could install, and every path that needed the
# server to hold a cluster identity — creating a Build, most immediately — was
# blocked on a ServiceAccount that could not exist.
ARG GO_VERSION=1.27.0
FROM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

# CGO off so the result runs on distroless/static. The panel's assets are
# embedded by package panel, so nothing has to be copied alongside the binary —
# which is also why there is no COPY of a web root below.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o damga cmd/damga/main.go

# Same base and the same reasoning as the operator's: a CA bundle, /etc/passwd,
# and nothing an attacker who lands here can call.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/damga .
USER 65532:65532

ENTRYPOINT ["/damga"]
