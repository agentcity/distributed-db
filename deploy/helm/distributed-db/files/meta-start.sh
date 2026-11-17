#!/bin/sh
# Start one replicated meta/TSO node.
#
# The pod ordinal is parsed from POD_NAME; peer addresses are built from the
# same headless service so a StatefulSet scale does not change any node ID.
# The script is POSIX sh (the image is Alpine-based) and fails closed: missing
# environment variables abort startup instead of silently forming a cluster
# with the wrong peers.
set -eu

: "${POD_NAME:?POD_NAME is required}"
: "${POD_NAMESPACE:?POD_NAMESPACE is required}"
: "${META_PORT:?META_PORT is required}"
: "${META_REPLICAS:?META_REPLICAS is required}"
: "${META_POD_PREFIX:?META_POD_PREFIX is required}"
: "${META_HEADLESS:?META_HEADLESS is required}"
: "${CLUSTER_DOMAIN:?CLUSTER_DOMAIN is required}"

SELF="${POD_NAME}.${META_HEADLESS}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}:${META_PORT}"
PEERS=""
i=0
while [ "$i" -lt "${META_REPLICAS}" ]; do
  ADDR="${META_POD_PREFIX}-${i}.${META_HEADLESS}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}:${META_PORT}"
  if [ "$ADDR" != "$SELF" ]; then
    if [ -z "$PEERS" ]; then
      PEERS="$ADDR"
    else
      PEERS="${PEERS},${ADDR}"
    fi
  fi
  i=$((i + 1))
done

set -- /app/kv2 \
  -mode=meta \
  -addr=":${META_PORT}" \
  -meta-id="${SELF}" \
  -meta-peers="${PEERS}" \
  -meta-data-dir=/data/meta \
  -log-level="${LOG_LEVEL:-info}"

# Protects SetRanges/SplitRange/MergeRanges with a Bearer token. The token is a
# command-line flag because kv2 does not read it from the environment; it is
# therefore visible in the process arguments to anyone who can inspect the
# container. The Secret is the only stored copy.
if [ "${META_ADMIN_TOKEN_ENABLED:-false}" = "true" ]; then
  : "${META_ADMIN_TOKEN:?META_ADMIN_TOKEN is required when META_ADMIN_TOKEN_ENABLED=true}"
  set -- "$@" -admin-token="${META_ADMIN_TOKEN}"
fi

if [ "${TLS_ENABLED:-false}" = "true" ]; then
  : "${TLS_DIR:?TLS_DIR is required when TLS_ENABLED=true}"
  : "${TLS_CERT_FILE:?TLS_CERT_FILE is required when TLS_ENABLED=true}"
  : "${TLS_KEY_FILE:?TLS_KEY_FILE is required when TLS_ENABLED=true}"
  : "${TLS_CA_FILE:?TLS_CA_FILE is required when TLS_ENABLED=true}"
  set -- "$@" \
    -tls-cert="${TLS_DIR}/${TLS_CERT_FILE}" \
    -tls-key="${TLS_DIR}/${TLS_KEY_FILE}" \
    -tls-ca="${TLS_DIR}/${TLS_CA_FILE}"
  if [ "${MTLS_ENABLED:-false}" = "true" ]; then
    set -- "$@" -tls-client-ca="${TLS_DIR}/${TLS_CA_FILE}"
  fi
fi

exec "$@"
