#!/bin/sh
# =============================================================================
# compose-bootstrap.sh — one-shot, idempotent v2 range-map bootstrap
# =============================================================================
#
# Used by the compose `bootstrap` service after the meta and KV healthchecks
# pass. It performs a bounded, fail-closed bootstrap:
#
#   1. wait (bounded) for a meta leader;
#   2. call `kvctl init-ranges` with the configured KV replica set;
#   3. verify the resulting range map through `kvctl ranges`.
#
# `init-ranges` itself is fail-closed: it only bootstraps a fresh meta with a
# single range and no replicas, refuses to replace a split map or a different
# replica set, and a second run with the same replica set is a no-op. This
# script preserves that contract and adds bounded retries for a leader move.
#
# Security:
#   * the admin token is read from the environment only and exported as
#     KVCTL_ADMIN_TOKEN (kvctl's documented env form). It is never passed as
#     an argv element and never printed by this script;
#   * no default token is set here. Empty/unset means kvctl sends no token,
#     which matches the development compose stacks where kv2 runs with an
#     empty -admin-token;
#   * TLS is optional and opt-in; plaintext is the compose default.
#
# Required environment:
#   META_ADDRS       comma-separated meta gRPC addresses (or META_ADDR for one)
#   KV_REPLICAS      comma-separated KV replica addresses, host:port
#
# Optional environment:
#   META_ADDR        single meta address when META_ADDRS is unset
#   META_REPLICAS    expected meta group size (default 1); >1 requires a
#                    leader to be elected before init-ranges is allowed
#   WAIT_TIMEOUT     seconds to wait for a leader (default 180)
#   WAIT_INTERVAL    seconds between attempts (default 2)
#   INIT_ATTEMPTS    bounded init-ranges attempts (default 3)
#   VERIFY_ATTEMPTS  bounded range verification attempts (default 10)
#   VERIFY_INTERVAL  seconds between verification attempts (default 1)
#   KVCTL_TIMEOUT    per-command timeout (default 10s)
#   KVCTL            kvctl path (default /app/kvctl)
#   KVCTL_ADMIN_TOKEN / META_ADMIN_TOKEN
#                    admin token for init-ranges; META_ADMIN_TOKEN is copied to
#                    KVCTL_ADMIN_TOKEN when the latter is unset
#   KVCTL_TLS_CA / KVCTL_TLS_CERT / KVCTL_TLS_KEY / KVCTL_TLS_SERVER_NAME
#                    optional TLS files/name; any non-empty value enables TLS
#
# Exit status: 0 on a verified (or already completed) range map, non-zero on a
# timeout, an init-ranges refusal, or verification failure.
# =============================================================================
set -eu

log() {
  printf '%s compose-bootstrap: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

fail() {
  log "ERROR: $*" >&2
  exit 1
}

is_uint() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

META_ADDRS=${META_ADDRS:-${META_ADDR:-}}
[ -n "$META_ADDRS" ] || fail "META_ADDRS (or META_ADDR) is required"
KV_REPLICAS=${KV_REPLICAS:-}
[ -n "$KV_REPLICAS" ] || fail "KV_REPLICAS is required"

META_REPLICAS=${META_REPLICAS:-1}
WAIT_TIMEOUT=${WAIT_TIMEOUT:-180}
WAIT_INTERVAL=${WAIT_INTERVAL:-2}
INIT_ATTEMPTS=${INIT_ATTEMPTS:-3}
VERIFY_ATTEMPTS=${VERIFY_ATTEMPTS:-10}
VERIFY_INTERVAL=${VERIFY_INTERVAL:-1}
KVCTL_TIMEOUT=${KVCTL_TIMEOUT:-10s}
KVCTL=${KVCTL:-/app/kvctl}

is_uint "$META_REPLICAS" || fail "META_REPLICAS must be an integer (got '$META_REPLICAS')"
is_uint "$WAIT_TIMEOUT" || fail "WAIT_TIMEOUT must be an integer (got '$WAIT_TIMEOUT')"
is_uint "$WAIT_INTERVAL" || fail "WAIT_INTERVAL must be an integer (got '$WAIT_INTERVAL')"
is_uint "$INIT_ATTEMPTS" || fail "INIT_ATTEMPTS must be an integer (got '$INIT_ATTEMPTS')"
is_uint "$VERIFY_ATTEMPTS" || fail "VERIFY_ATTEMPTS must be an integer (got '$VERIFY_ATTEMPTS')"
is_uint "$VERIFY_INTERVAL" || fail "VERIFY_INTERVAL must be an integer (got '$VERIFY_INTERVAL')"
[ "$META_REPLICAS" -gt 0 ] || fail "META_REPLICAS must be > 0"
[ "$WAIT_TIMEOUT" -gt 0 ] || fail "WAIT_TIMEOUT must be > 0"
[ "$WAIT_INTERVAL" -gt 0 ] || fail "WAIT_INTERVAL must be > 0"
[ "$INIT_ATTEMPTS" -gt 0 ] || fail "INIT_ATTEMPTS must be > 0"
[ "$VERIFY_ATTEMPTS" -gt 0 ] || fail "VERIFY_ATTEMPTS must be > 0"
[ "$VERIFY_INTERVAL" -gt 0 ] || fail "VERIFY_INTERVAL must be > 0"
[ -x "$KVCTL" ] || fail "kvctl is not executable at $KVCTL"

# Normalize comma lists; empty entries are rejected by kvctl itself.
META_ADDRS=$(printf '%s' "$META_ADDRS" | tr -d ' \t\r\n')
KV_REPLICAS=$(printf '%s' "$KV_REPLICAS" | tr -d ' \t\r\n')
[ -n "$META_ADDRS" ] || fail "META_ADDRS is empty after normalization"
[ -n "$KV_REPLICAS" ] || fail "KV_REPLICAS is empty after normalization"
case ",$KV_REPLICAS," in
  *,,*) fail "KV_REPLICAS contains an empty address" ;;
esac

# Keep the credential in the environment only. kvctl reads KVCTL_ADMIN_TOKEN
# as its -admin-token default; it is never echoed or placed in argv.
if [ -z "${KVCTL_ADMIN_TOKEN:-}" ] && [ -n "${META_ADMIN_TOKEN:-}" ]; then
  KVCTL_ADMIN_TOKEN=$META_ADMIN_TOKEN
fi
if [ -n "${KVCTL_ADMIN_TOKEN:-}" ]; then
  # Ensure a token supplied as a shell variable (not just via `VAR=x cmd`) is
  # visible to the kvctl child process. It is never placed in argv.
  export KVCTL_ADMIN_TOKEN
fi
unset META_ADMIN_TOKEN 2>/dev/null || true

TLS_CA=${KVCTL_TLS_CA:-}
TLS_CERT=${KVCTL_TLS_CERT:-}
TLS_KEY=${KVCTL_TLS_KEY:-}
TLS_SERVER_NAME=${KVCTL_TLS_SERVER_NAME:-}
TLS_ENABLED=false
if [ -n "$TLS_CA$TLS_CERT$TLS_KEY$TLS_SERVER_NAME" ]; then
  TLS_ENABLED=true
fi

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/compose-bootstrap.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

# kvctl_json runs one meta-only kvctl command and emits JSON. TLS flags are
# added only when configured; the admin token travels in the environment.
kvctl_json() {
  _addr=$1
  shift
  if [ "$TLS_ENABLED" = "true" ]; then
    "$KVCTL" -meta "$_addr" -timeout "$KVCTL_TIMEOUT" -format json \
      -tls-ca "$TLS_CA" -tls-cert "$TLS_CERT" -tls-key "$TLS_KEY" \
      -tls-server-name "$TLS_SERVER_NAME" "$@"
  else
    "$KVCTL" -meta "$_addr" -timeout "$KVCTL_TIMEOUT" -format json "$@"
  fi
}

# discover_leader_once probes every meta address. In a replicated group a
# follower's status RPC fails closed (requireLeader), so a successful status
# identifies the leader; in standalone mode (META_REPLICAS=1) an empty
# meta_leader is expected and the probed address is used for writes.
discover_leader_once() {
  _addrs=$(printf '%s' "$META_ADDRS" | tr ',' ' ')
  for _addr in $_addrs; do
    if _out=$(kvctl_json "$_addr" status 2>"$TMP_DIR/status.err"); then
      _cand=$(printf '%s\n' "$_out" |
        sed -n 's/.*"meta_leader":"\([^"]*\)".*/\1/p' | head -n 1)
      if [ "$META_REPLICAS" -le 1 ] || [ -n "$_cand" ]; then
        if [ -n "$_cand" ]; then
          LEADER=$_cand
        else
          LEADER=$_addr
        fi
        return 0
      fi
      printf 'meta at %s is reachable but has no leader yet\n' "$_addr" \
        >"$TMP_DIR/status.err"
    fi
  done
  return 1
}

LEADER=""
attempt=0
deadline=$(( $(date +%s) + WAIT_TIMEOUT ))
while :; do
  attempt=$((attempt + 1))
  if discover_leader_once; then
    log "meta ready: leader=${LEADER} (attempt ${attempt})"
    break
  fi
  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    log "timed out after ${WAIT_TIMEOUT}s waiting for a meta leader via ${META_ADDRS}"
    sed 's/^/  /' "$TMP_DIR/status.err" >&2 2>/dev/null || true
    exit 1
  fi
  log "waiting for meta leader (attempt ${attempt})"
  sleep "$WAIT_INTERVAL"
done

# init-ranges is idempotent. Retry a bounded number of times so a leader move
# between discovery and the RPC does not fail the one-shot job permanently; a
# deterministic refusal (split map / different replicas) fails fast on the
# final attempt with kvctl's diagnostic.
init_ok=0
init_attempt=0
while [ "$init_attempt" -lt "$INIT_ATTEMPTS" ]; do
  init_attempt=$((init_attempt + 1))
  if _out=$(kvctl_json "$LEADER" init-ranges -replicas "$KV_REPLICAS" \
      2>"$TMP_DIR/init.err"); then
    log "init-ranges: $(printf '%s' "$_out" | tr '\n' ' ')"
    init_ok=1
    break
  fi
  log "init-ranges attempt ${init_attempt}/${INIT_ATTEMPTS} failed: $(sed -n '1p' "$TMP_DIR/init.err" 2>/dev/null || true)"
  if [ "$init_attempt" -lt "$INIT_ATTEMPTS" ]; then
    if discover_leader_once; then
      log "retrying init-ranges against leader=${LEADER}"
    else
      log "no meta leader visible; retrying discovery"
    fi
    sleep "$WAIT_INTERVAL"
  fi
done
[ "$init_ok" -eq 1 ] || fail "init-ranges failed after ${INIT_ATTEMPTS} attempt(s)"

# Verification is intentionally separate from init-ranges' own post-write
# check: a split/merge tool must never see an unverified descriptor.
verify_ranges() {
  if ! _out=$(kvctl_json "$LEADER" ranges 2>"$TMP_DIR/verify.err"); then
    return 1
  fi
  printf '%s\n' "$_out" >"$TMP_DIR/ranges.json"
  _count=$(grep -o '"id":' "$TMP_DIR/ranges.json" | wc -l | tr -d ' ')
  if [ "$_count" -ne 1 ]; then
    log "expected exactly 1 range, found ${_count}"
    return 1
  fi
  _actual=$(sed -n 's/.*"replicas":\[\([^]]*\)\].*/\1/p' "$TMP_DIR/ranges.json" |
    tr ',' '\n' | tr -d ' "' | sed '/^$/d' | sort)
  _expected=$(printf '%s' "$KV_REPLICAS" | tr ',' '\n' | tr -d ' ' |
    sed '/^$/d' | sort)
  if [ "$_actual" != "$_expected" ]; then
    log "range replica set mismatch"
    log "expected: ${_expected}"
    log "actual:   ${_actual}"
    return 1
  fi
  return 0
}

verify_attempt=0
verified=0
while [ "$verify_attempt" -lt "$VERIFY_ATTEMPTS" ]; do
  verify_attempt=$((verify_attempt + 1))
  if verify_ranges; then
    verified=1
    break
  fi
  log "range map not converged yet (attempt ${verify_attempt}/${VERIFY_ATTEMPTS})"
  sleep "$VERIFY_INTERVAL"
done
[ "$verified" -eq 1 ] || fail "range map verification failed after ${VERIFY_ATTEMPTS} attempt(s)"

log "verified range map:"
sed 's/^/  /' "$TMP_DIR/ranges.json"
log "bootstrap complete: meta=${LEADER} replicas=${KV_REPLICAS}"
