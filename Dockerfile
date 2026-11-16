# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.23-bookworm AS build

WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary: no cgo, no timestamps, no debug symbols.
# The result runs on Alpine as well as on distroless/static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/db ./cmd/db

# ---- runtime stage ---------------------------------------------------------
# Alpine (instead of distroless) is deliberate: the busybox `wget` already in
# the image lets Compose healthchecks hit /health without extra tooling.
FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 db \
    && adduser -S -u 10001 -G db -h /app db \
    && mkdir -p /app /data \
    && chown -R 10001:10001 /app /data

COPY --from=build --chown=10001:10001 /out/db /app/db

USER 10001:10001
WORKDIR /app

# 7001/8001 are the node defaults; 8080/9090 are the coordinator defaults.
# `docker compose` maps these ports explicitly, EXPOSE is documentation only.
EXPOSE 7001 8001 8080 9090

ENTRYPOINT ["/app/db"]
