# Build the manager binary
# Pinned rather than tracking a major line, so a rebuild months from now
# produces the same binary. Kept in step with the go directive in go.mod.
ARG GO_VERSION=1.26.0
FROM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
# distroless/static holds a CA bundle, /etc/passwd and nothing else — no shell,
# no package manager, nothing for an attacker who lands here to call.
#
# :nonroot is a moving tag, which this project treats as a problem everywhere
# else. It is accepted here because what a cluster admits is the published
# manifest by digest, signed by the pipeline: the base moving changes the next
# build, never a release already running. Trivy runs against the result on every
# push, so a base that moves somewhere bad fails the build rather than shipping.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
