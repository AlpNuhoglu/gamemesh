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
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.24@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS build
WORKDIR /src
COPY go.mod go.sum ./
# Cache mounts keep the module and build caches between builds, so a code
# change recompiles only the packages it touches.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG SERVICE
ARG TARGETOS TARGETARCH
# The commit this image is built from; /health and traces report it.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE" || { echo "set --build-arg SERVICE=<name of a cmd/ dir>" >&2; exit 1; }; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/alpnuhoglu/gamemesh/pkg/config.version=$VERSION" \
      -o /out/service "./cmd/$SERVICE"

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=build /out/service /service
# Numeric, so Kubernetes' runAsNonRoot can verify the user is not root; no
# passwd entry is needed for a static binary.
USER 10001:10001
ENTRYPOINT ["/service"]
