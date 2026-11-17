#!/bin/sh
# Start the pgwire gateway.
#
# pgsrv takes its password only as a command-line flag, so the wrapper expands
# the Secret-backed environment variable here. The password is therefore still
# visible in the process arguments to anyone who can inspect the container
# (the binary documents this limitation). There is no environment-only flag.
set -eu

: "${PG_PORT:?PG_PORT is required}"
: "${PG_METRICS_PORT:?PG_METRICS_PORT is required}"
: "${META_SERVICE:?META_SERVICE is required}"
: "${META_PORT:?META_PORT is required}"
: "${KV_NODES:?KV_NODES is required}"

set -- /app/pgsrv \
  -addr=":${PG_PORT}" \
  -metrics-addr=":${PG_METRICS_PORT}" \
  -meta="${META_SERVICE}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}:${META_PORT}" \
  -nodes="${KV_NODES}" \
  -log-level="${LOG_LEVEL:-info}"

if [ "${TLS_ENABLED:-false}" = "true" ]; then
  : "${TLS_DIR:?TLS_DIR is required when TLS_ENABLED=true}"
  : "${TLS_CERT_FILE:?TLS_CERT_FILE is required when TLS_ENABLED=true}"
  : "${TLS_KEY_FILE:?TLS_KEY_FILE is required when TLS_ENABLED=true}"
  : "${TLS_CA_FILE:?TLS_CA_FILE is required when TLS_ENABLED=true}"
  set -- "$@" \
    -tls-cert="${TLS_DIR}/${TLS_CERT_FILE}" \
    -tls-key="${TLS_DIR}/${TLS_KEY_FILE}" \
    -tls-ca="${TLS_DIR}/${TLS_CA_FILE}"
fi

if [ "${PG_AUTH_ENABLED:-false}" = "true" ]; then
  : "${PG_AUTH_USER:?PG_AUTH_USER is required when PG_AUTH_ENABLED=true}"
  : "${PG_AUTH_PASSWORD:?PG_AUTH_PASSWORD is required when PG_AUTH_ENABLED=true}"
  set -- "$@" \
    -auth-user="${PG_AUTH_USER}" \
    -auth-password="${PG_AUTH_PASSWORD}"
fi

exec "$@"
