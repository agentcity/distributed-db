#!/bin/sh
# One-time meta range-map bootstrap for a fresh cluster.
#
# The chart runs this from a post-install Job after the meta StatefulSet is
# reachable. It is deliberately fail-closed:
#
#   * it waits for a meta leader before touching the map;
#   * it calls `kvctl init-ranges`, which only bootstraps a single range that
#     has NO replicas and refuses to replace a split or differently-placed map;
#   * a second run with the same replica set is a no-op (idempotent).
#
# The admin token is passed through KVCTL_ADMIN_TOKEN, not as a command-line
# argument, so it is not visible in the process list. The token is never
# printed.
#
# Required environment:
#   META_ADDR        fully qualified meta service address, host:port
#   KV_REPLICAS      comma-separated KV replica addresses, host:port
#
# Optional environment:
#   META_REPLICAS    expected meta group size (default 1). Values > 1 make the
#                    wait require an elected meta leader, not just a listener.
#   WAIT_TIMEOUT     seconds to wait for a meta leader (default 300)
#   WAIT_INTERVAL    seconds between readiness attempts (default 3)
#   KVCTL_TIMEOUT    per-command timeout (default 10s)
#   KVCTL            kvctl binary (default /app/kvctl)
#   TLS_ENABLED      "true" to use the mounted cluster TLS material
#   TLS_DIR          directory containing the certificate files
#   TLS_CERT_FILE    client/server certificate file name
#   TLS_KEY_FILE     private key file name
#   TLS_CA_FILE      CA bundle file name
#   META_ADMIN_TOKEN_ENABLED  "true" to require and send a Bearer token
#   META_ADMIN_TOKEN            admin token value (never logged)
set -eu

: "${META_ADDR:?META_ADDR is required}"
: "${KV_REPLICAS:?KV_REPLICAS is required}"

WAIT_TIMEOUT=${WAIT_TIMEOUT:-300}
WAIT_INTERVAL=${WAIT_INTERVAL:-3}
META_REPLICAS=${META_REPLICAS:-1}
KVCTL_TIMEOUT=${KVCTL_TIMEOUT:-10s}
KVCTL=${KVCTL:-/app/kvctl}
TLS_ENABLED=${TLS_ENABLED:-false}
META_ADMIN_TOKEN_ENABLED=${META_ADMIN_TOKEN_ENABLED:-false}

case "$WAIT_TIMEOUT" in
  ''|*[!0-9]*) echo "range-bootstrap: WAIT_TIMEOUT must be an integer (got '$WAIT_TIMEOUT')" >&2; exit 2 ;;
esac
case "$WAIT_INTERVAL" in
  ''|*[!0-9]*) echo "range-bootstrap: WAIT_INTERVAL must be an integer (got '$WAIT_INTERVAL')" >&2; exit 2 ;;
esac
case "$META_REPLICAS" in
  ''|*[!0-9]*) echo "range-bootstrap: META_REPLICAS must be an integer (got '$META_REPLICAS')" >&2; exit 2 ;;
esac
if [ "$WAIT_TIMEOUT" -le 0 ] || [ "$WAIT_INTERVAL" -le 0 ] || [ "$META_REPLICAS" -le 0 ]; then
  echo "range-bootstrap: WAIT_TIMEOUT, WAIT_INTERVAL and META_REPLICAS must be > 0" >&2
  exit 2
fi
if [ ! -x "$KVCTL" ]; then
  echo "range-bootstrap: kvctl is not executable at $KVCTL" >&2
  exit 2
fi

log() {
  printf '%s range-bootstrap: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

# kvctl_exec runs one kvctl command with the common connection flags. It keeps
# the admin token in the environment so it does not appear in argv.
kvctl_exec() {
  if [ "$META_ADMIN_TOKEN_ENABLED" = "true" ]; then
    : "${META_ADMIN_TOKEN:?META_ADMIN_TOKEN is required when META_ADMIN_TOKEN_ENABLED=true}"
    KVCTL_ADMIN_TOKEN=$META_ADMIN_TOKEN
    export KVCTL_ADMIN_TOKEN
  fi

  if [ "$TLS_ENABLED" = "true" ]; then
    : "${TLS_DIR:?TLS_DIR is required when TLS_ENABLED=true}"
    : "${TLS_CERT_FILE:?TLS_CERT_FILE is required when TLS_ENABLED=true}"
    : "${TLS_KEY_FILE:?TLS_KEY_FILE is required when TLS_ENABLED=true}"
    : "${TLS_CA_FILE:?TLS_CA_FILE is required when TLS_ENABLED=true}"
    "$KVCTL" \
      -meta "$META_ADDR" \
      -nodes "$KV_REPLICAS" \
      -timeout "$KVCTL_TIMEOUT" \
      -tls-ca "${TLS_DIR}/${TLS_CA_FILE}" \
      -tls-cert "${TLS_DIR}/${TLS_CERT_FILE}" \
      -tls-key "${TLS_DIR}/${TLS_KEY_FILE}" \
      "$@"
  else
    "$KVCTL" \
      -meta "$META_ADDR" \
      -nodes "$KV_REPLICAS" \
      -timeout "$KVCTL_TIMEOUT" \
      "$@"
  fi
}

deadline=$(( $(date +%s) + WAIT_TIMEOUT ))
attempt=0
while :; do
  attempt=$((attempt + 1))
  status_ok=0
  leader=""
  if kvctl_exec status > /tmp/range-bootstrap-status.txt 2> /tmp/range-bootstrap-status.err; then
    status_ok=1
    # The status table is tab-separated. cut -f1 keeps an empty first field
    # empty, which awk on whitespace would not.
    leader=$(sed -n '2p' /tmp/range-bootstrap-status.txt | cut -f1)
    if [ "$META_REPLICAS" -le 1 ] || [ -n "$leader" ]; then
      log "meta is ready: leader=${leader:-<standalone>} ranges=$(sed -n '2p' /tmp/range-bootstrap-status.txt | cut -f3)"
      break
    fi
  fi
  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    log "timed out after ${WAIT_TIMEOUT}s waiting for meta ${META_ADDR}"
    if [ "$status_ok" -eq 1 ]; then
      log "meta is reachable but has no leader after ${attempt} attempts; refusing to write the range map"
    fi
    sed 's/^/  /' /tmp/range-bootstrap-status.err >&2 || true
    exit 1
  fi
  if [ "$status_ok" -eq 1 ]; then
    log "meta is reachable but has no leader yet (attempt ${attempt})"
  else
    log "waiting for meta ${META_ADDR} (attempt ${attempt})"
  fi
  sleep "$WAIT_INTERVAL"
done

# init-ranges is idempotent and verifies the result itself. On a split map or
# a different existing replica set it exits non-zero without changing anything;
# the Job then retries according to backoffLimit, and a persistent mismatch
# fails the install rather than rewriting production metadata.
log "initializing range map with replicas: ${KV_REPLICAS}"
if ! kvctl_exec init-ranges -replicas "$KV_REPLICAS"; then
  log "init-ranges failed; the range map was not changed"
  exit 1
fi

log "range map after bootstrap:"
kvctl_exec ranges || true
log "bootstrap complete"
