#!/bin/sh
# Start the SQL-over-HTTPS gateway.
#
# sqlsrv serves the SQL API, /metrics, /debug/slo and /healthz on one TLS
# listener. The same certificate is presented to meta/KV for mTLS. The admin
# token is passed as a flag because sqlsrv has no environment-only option; it
# is visible in the container's process arguments to anyone who can inspect
# that container (documented limitation).
set -eu

: "${POD_NAMESPACE:?POD_NAMESPACE is required}"
: "${SQL_PORT:?SQL_PORT is required}"
: "${META_SERVICE:?META_SERVICE is required}"
: "${META_PORT:?META_PORT is required}"
: "${KV_NODES:?KV_NODES is required}"
: "${CLUSTER_DOMAIN:?CLUSTER_DOMAIN is required}"

set -- /app/sqlsrv \
  -http-addr=":${SQL_PORT}" \
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

if [ "${SQL_AUTH_ENABLED:-false}" = "true" ]; then
  : "${SQL_AUTH_TOKEN:?SQL_AUTH_TOKEN is required when SQL_AUTH_ENABLED=true}"
  set -- "$@" -auth-token="${SQL_AUTH_TOKEN}"
fi

exec "$@"
