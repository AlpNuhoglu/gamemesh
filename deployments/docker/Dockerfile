# syntax=docker/dockerfile:1
# One Dockerfile for every service:
#   docker build --build-arg SERVICE=gateway -f deployments/docker/Dockerfile .
#
# Multi-stage and multi-arch. The build stage always runs on the builder's own
# platform (--platform=$BUILDPLATFORM) and Go cross-compiles for the target
# via GOOS/GOARCH, so an amd64 image built on an arm64 Mac needs no CPU
# emulation; the final stage has no RUN step for the same reason. CGO stays
# off: the static binary runs on Alpine's musl, a glibc-linked one would not
# start. Final image ~40 MB (≈32 MB binary on ≈9 MB Alpine), with busybox
# wget for container healthchecks.
#
# Both bases are pinned by digest as well as tag: a tag can be republished
# with other contents, the digest cannot. Each is the multi-platform index
# digest, so the pin holds for every target architecture. Dependabot bumps
# tag and digest together; the golang tag also has to match go.mod's
# toolchain line.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.24@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS modules
WORKDIR /src
COPY go.mod go.sum ./
# The modules go into this layer, not into a cache mount: CI's layer cache
# (GitHub Actions) keeps layers but not the contents of cache mounts, so with
# a mount every CI build fetched every module again, and a proxy error failed
# it (finding #99). The layer changes only with go.mod and go.sum. It is a
# stage of its own because two builds of it differ (Go's telemetry files,
# directory times): CI builds it once, in the go-modules job, and every
# service's build takes that one from the cache instead of making its own
# 200 MiB copy.
RUN go mod download

FROM modules AS build
COPY . .
ARG SERVICE
ARG TARGETOS TARGETARCH
# The commit this image is built from; /health and traces report it.
ARG VERSION=dev
# The build cache stays a cache mount (kept between local builds, so a code
# change recompiles only the packages it touches). GOPROXY=off: every module
# must come from the layer above; a missing one fails the build instead of
# being fetched.
RUN --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE" || { echo "set --build-arg SERVICE=<name of a cmd/ dir>" >&2; exit 1; }; \
    CGO_ENABLED=0 GOPROXY=off GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/alpnuhoglu/gamemesh/pkg/config.version=$VERSION" \
      -o /out/service "./cmd/$SERVICE"

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
COPY --from=build /out/service /service
# Numeric, so Kubernetes' runAsNonRoot can verify the user is not root; no
# passwd entry is needed for a static binary.
USER 10001:10001
ENTRYPOINT ["/service"]
