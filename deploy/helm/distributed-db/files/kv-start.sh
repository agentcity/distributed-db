#!/bin/sh
# Start one KV (Multi-Raft data plane) node.
#
# The node ID is its stable pod DNS name, never the pod IP. Raft peer lists are
# built from the headless service; the local node is excluded.
set -eu

: "${POD_NAME:?POD_NAME is required}"
: "${POD_NAMESPACE:?POD_NAMESPACE is required}"
: "${KV_PORT:?KV_PORT is required}"
: "${KV_REPLICAS:?KV_REPLICAS is required}"
: "${KV_POD_PREFIX:?KV_POD_PREFIX is required}"
: "${KV_HEADLESS:?KV_HEADLESS is required}"
: "${CLUSTER_DOMAIN:?CLUSTER_DOMAIN is required}"

SELF="${POD_NAME}.${KV_HEADLESS}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}:${KV_PORT}"
PEERS=""
i=0
while [ "$i" -lt "${KV_REPLICAS}" ]; do
  ADDR="${KV_POD_PREFIX}-${i}.${KV_HEADLESS}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}:${KV_PORT}"
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
  -mode=node \
  -raft-addr=":${KV_PORT}" \
  -id="${SELF}" \
  -peers="${PEERS}" \
  -data-dir=/data \
  -durability="${DURABILITY:-sync}" \
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
  if [ "${MTLS_ENABLED:-false}" = "true" ]; then
    set -- "$@" -tls-client-ca="${TLS_DIR}/${TLS_CA_FILE}"
  fi
fi

exec "$@"
