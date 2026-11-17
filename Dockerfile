# syntax=docker/dockerfile:1.7
#
# Multi-binary production image for the v2 distributed-db stack.
#
# The image contains every binary the Helm chart starts or documents:
#
#   /app/kv2       meta/TSO (-mode=meta) and Multi-Raft KV node (-mode=node)
#   /app/sqlsrv    SQL-over-HTTPS gateway
#   /app/pgsrv     PostgreSQL wire-protocol gateway
#   /app/kvctl     terminal client, range admin and backup client
#   /app/kvbackup  logical backup/restore/verify CLI (TLS/admin-token capable)
#   /app/edgehash  immutable edge hash-set builder/server/client
#   /app/db        legacy v1 coordinator/node (kept for docker-compose.v1.yml)
#
# The legacy /app/db binary remains the default ENTRYPOINT so the existing
# docker-compose.v1.yml keeps working unchanged. The Helm chart always overrides
# the command and executes one of the explicit paths above; operators running
# the image directly should do the same, e.g.:
#
#   docker run --rm --entrypoint /app/kv2 <image> -mode=meta -addr=:9090
#   docker run --rm --entrypoint /app/kvctl <image> -meta <host>:9090 status
#
# The runtime stage is Alpine because the chart's startup scripts need a POSIX
# shell (/bin/sh) and the compose healthcheck uses busybox wget. It is not
# distroless; distroless would break both.
#
# Reproducibility: -trimpath, -buildvcs=false and no build timestamps keep the
# binaries independent of the checkout path and of the local git state. The
# VERSION/REVISION build args are metadata only (labels); they do not change
# the compiled code.
#
# Multi-arch: the build stage is not pinned with --platform. Classic
# `docker build` (and some Docker/buildx combinations) leave BUILDPLATFORM
# empty, which makes `FROM --platform=$BUILDPLATFORM` a parse error. Instead the
# stage runs on the target platform and the Go toolchain cross-compiles with
# the explicit TARGETOS/TARGETARCH values that BuildKit passes per platform;
# CGO_ENABLED=0 makes those binaries self-contained. A native build with no
# platform arguments falls back to the Go toolchain's own GOOS/GOARCH, which is
# exactly the platform the classic builder is running on.

ARG GO_VERSION=1.23

# ---- build stage -----------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build

# BuildKit fills these in from --platform. Redeclaring them without a value
# keeps the automatic values. When they are absent (classic builder), they are
# derived from TARGETPLATFORM or, for a plain native build, from `go env`.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG TARGETPLATFORM
ARG VERSION=dev
ARG REVISION=unknown

WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build every binary in one layer. A single `go build` invocation cannot write
# multiple executables to one -o path, so loop over the commands. The legacy
# cmd/db is built too (compose compatibility, see the header).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    target_os=${TARGETOS:-}; \
    target_arch=${TARGETARCH:-}; \
    if [ -z "$target_arch" ] && [ -n "${TARGETPLATFORM:-}" ]; then \
      target_arch=${TARGETPLATFORM#*/}; \
      target_arch=${target_arch%%/*}; \
    fi; \
    if [ -z "$target_os" ] && [ -n "${TARGETPLATFORM:-}" ]; then \
      target_os=${TARGETPLATFORM%%/*}; \
    fi; \
    if [ -z "$target_os" ]; then target_os=$(go env GOOS); fi; \
    if [ -z "$target_arch" ]; then target_arch=$(go env GOARCH); fi; \
    if [ -z "$target_os" ] || [ -z "$target_arch" ]; then \
      echo "error: cannot determine target platform (TARGETOS/TARGETARCH empty and go env failed)" >&2; \
      exit 1; \
    fi; \
    echo "build platform=$(go env GOOS)/$(go env GOARCH) target=${target_os}/${target_arch} variant=${TARGETVARIANT:-none}"; \
    mkdir -p /out/bin; \
    for cmd in db kv2 sqlsrv pgsrv kvctl kvbackup edgehash; do \
      echo "building ./cmd/$cmd for ${target_os}/${target_arch}"; \
      CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -buildvcs=false -ldflags "-s -w" \
          -o "/out/bin/$cmd" "./cmd/$cmd"; \
    done; \
    for cmd in db kv2 sqlsrv pgsrv kvctl kvbackup edgehash; do \
      test -x "/out/bin/$cmd"; \
    done; \
    ls -l /out/bin

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.20 AS runtime

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="distributed-db" \
      org.opencontainers.image.description="distributed-db v2 multi-binary runtime" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

# ca-certificates is required for outbound TLS (meta/KV peers, S3/backup
# endpoints, edgehash sync). tzdata is intentionally absent: servers log UTC
# and operators can set TZ explicitly.
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 db \
    && adduser -S -u 10001 -G db -h /app db \
    && mkdir -p /app /data \
    && chown -R 10001:10001 /app /data

COPY --from=build --chown=10001:10001 /out/bin/ /app/

USER 10001:10001
WORKDIR /app

# Documentation only; docker compose and the chart set the actual ports.
#   7001 kv2 node, 9090 kv2 meta, 8080 sqlsrv, 5432 pgsrv, 9187 pgsrv metrics,
#   8001 legacy cmd/db node HTTP.
EXPOSE 7001 9090 8080 5432 9187 8001

# Backwards-compatible default. Chart pods override `command`; run the v2
# binaries explicitly when using the image directly (see the header).
ENTRYPOINT ["/app/db"]
