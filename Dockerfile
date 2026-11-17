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
#   /app/kvbackup  logical backup/restore/verify CLI (plaintext only)
#   /app/edgehash  immutable edge hash-set builder/server/client
#   /app/db        legacy v1 coordinator/node (kept for docker-compose.yml)
#
# The legacy /app/db binary remains the default ENTRYPOINT so the existing
# docker-compose.yml keeps working unchanged. The Helm chart always overrides
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

ARG GO_VERSION=1.23

# ---- build stage -----------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH
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
    : "${TARGETARCH:=$(go env GOARCH)}"; \
    mkdir -p /out/bin; \
    for cmd in db kv2 sqlsrv pgsrv kvctl kvbackup edgehash; do \
      echo "building ./cmd/$cmd for ${TARGETOS:-linux}/$TARGETARCH"; \
      CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="$TARGETARCH" \
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
