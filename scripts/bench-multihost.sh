#!/usr/bin/env bash
#
# bench-multihost.sh - run the external-cluster multi-host SLO harness.
#
# This script does NOT start a meta service, KV node, container or database.
# It validates SLO_* environment variables, runs TestSLOMultiHost against the
# already-deployed cluster, captures the full output, and exits non-zero when
# the harness or a configured gate fails.
#
# Quick start (3 KV nodes; replace the addresses with your cluster):
#
#   export SLO_META=10.20.0.10:9090
#   export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
#   export SLO_CLIENTS=16
#   export SLO_DURATION=10m
#   export SLO_VALUE_SIZE=1024
#   export SLO_KEYSPACE=10000000
#   export SLO_P99_GATE_MS=50
#   ./scripts/bench-multihost.sh
#
# 5 KV nodes:
#
#   export SLO_META=10.20.0.10:9090
#   export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001
#   ./scripts/bench-multihost.sh
#
# 7 KV nodes: add 10.20.0.16:7001,10.20.0.17:7001 to SLO_NODES.
#
# Options:
#   --dry-run          validate the environment and print the effective config
#   --example 3|5|7    print an example environment for that topology and exit
#   -h, --help         show help
#
# Outputs:
#   $SLO_OUT_DIR/slo-multihost-<run-id>.log   full output
#   $SLO_REPORT_JSON (or $SLO_OUT_DIR/...json) machine-readable report
#
# Exit codes: 0 = pass, 1 = harness/gate failure, 2 = usage/config error.
#
# See docs/SLO-RUNBOOK.md before treating any result as production evidence.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  printf 'bench-multihost: %s\n' "$*" >&2
  exit 2
}

note() {
  printf '%s\n' "$*"
}

usage() {
  cat <<'EOF'
Usage: scripts/bench-multihost.sh [--dry-run] [--example 3|5|7]

Runs the external-cluster SLO test TestSLOMultiHost. The cluster must already
be running; this script never starts servers.

Required environment:
  SLO_META    meta/TSO gRPC host:port
  SLO_NODES   comma-separated KV gRPC host:port list (3, 5 or 7 recommended)

Common optional environment:
  SLO_CLIENTS, SLO_MAX_INFLIGHT, SLO_DURATION, SLO_VALUE_SIZE, SLO_KEYSPACE,
  SLO_READ_RATIO, SLO_VERIFY_RATE, SLO_VERIFY_SAMPLE, SLO_PRELOAD_GB,
  SLO_PRELOAD_WORKERS, SLO_OP_TIMEOUT, SLO_SEED, SLO_P95_GATE_MS,
  SLO_P99_GATE_MS, SLO_MAX_ERROR_RATE, SLO_MIN_THROUGHPUT, SLO_OUT_DIR,
  SLO_REPORT_JSON, SLO_RUN_ID, SLO_TEST_TIMEOUT, SLO_ALLOW_SMALL_CLUSTER.

Run with --dry-run to see the effective settings without touching the cluster.
EOF
}

show_example() {
  case "${1:-3}" in
    3)
      cat <<'EOF'
# 3 KV nodes (one replication group, RF=3) and an external meta/TSO service.
# Replace these addresses; this is configuration only, not a server starter.
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SLO_CLIENTS=16
export SLO_MAX_INFLIGHT=16
export SLO_DURATION=10m
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=10000000
export SLO_READ_RATIO=0.8
export SLO_VERIFY_RATE=0.01
export SLO_VERIFY_SAMPLE=32
export SLO_PRELOAD_GB=0
export SLO_P99_GATE_MS=50
export SLO_P95_GATE_MS=25
export SLO_MAX_ERROR_RATE=0
export SLO_MIN_THROUGHPUT=0
export SLO_OUT_DIR=./.tmp-slo
./scripts/bench-multihost.sh
EOF
      ;;
    5)
      cat <<'EOF'
# 5 KV nodes. Use a topology whose failure domain matches production.
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001
export SLO_CLIENTS=24
export SLO_MAX_INFLIGHT=24
export SLO_DURATION=15m
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=20000000
export SLO_READ_RATIO=0.8
export SLO_VERIFY_RATE=0.01
export SLO_VERIFY_SAMPLE=32
export SLO_PRELOAD_GB=0
export SLO_P99_GATE_MS=50
export SLO_P95_GATE_MS=25
export SLO_MAX_ERROR_RATE=0
export SLO_MIN_THROUGHPUT=0
export SLO_OUT_DIR=./.tmp-slo
./scripts/bench-multihost.sh
EOF
      ;;
    7)
      cat <<'EOF'
# 7 KV nodes.
export SLO_META=10.20.0.10:9090
export SLO_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001,10.20.0.16:7001,10.20.0.17:7001
export SLO_CLIENTS=32
export SLO_MAX_INFLIGHT=32
export SLO_DURATION=15m
export SLO_VALUE_SIZE=1024
export SLO_KEYSPACE=30000000
export SLO_READ_RATIO=0.8
export SLO_VERIFY_RATE=0.01
export SLO_VERIFY_SAMPLE=32
export SLO_PRELOAD_GB=0
export SLO_P99_GATE_MS=50
export SLO_P95_GATE_MS=25
export SLO_MAX_ERROR_RATE=0
export SLO_MIN_THROUGHPUT=0
export SLO_OUT_DIR=./.tmp-slo
./scripts/bench-multihost.sh
EOF
      ;;
    *)
      die "--example expects 3, 5 or 7 (got '$1')"
      ;;
  esac
}

require_uint() {
  local name="$1" value="$2" min="$3" max="$4"
  awk -v v="$value" -v lo="$min" -v hi="$max" 'BEGIN {
    if (v !~ /^[0-9]+$/) exit 1
    if (v + 0 < lo + 0 || v + 0 > hi + 0) exit 1
  }' || die "$name must be an integer in [$min,$max] (got '$value')"
}

require_float() {
  local name="$1" value="$2" min="$3" max="$4"
  awk -v v="$value" -v lo="$min" -v hi="$max" 'BEGIN {
    if (v !~ /^[0-9]+([.][0-9]+)?$/) exit 1
    if (v + 0 < lo + 0 || v + 0 > hi + 0) exit 1
  }' || die "$name must be a number in [$min,$max] (got '$value')"
}

require_duration() {
  local name="$1" value="$2"
  if ! printf '%s\n' "$value" | grep -Eq '^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$|^[0-9]+([.][0-9]+)?$'; then
    die "$name must be a Go duration such as 30s or 5m, or seconds (got '$value')"
  fi
}

DRY_RUN=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --example)
      [ "$#" -ge 2 ] || die "--example requires 3, 5 or 7"
      show_example "$2"
      exit 0
      ;;
    --example=*)
      show_example "${1#*=}"
      exit 0
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
done

# ---- required environment -------------------------------------------------

[ -n "${SLO_META:-}" ] || die "SLO_META is required (meta/TSO gRPC host:port; try --example 3)"
[ -n "${SLO_NODES:-}" ] || die "SLO_NODES is required (comma-separated KV gRPC host:port list; try --example 3)"

case "$SLO_META" in
  *:*) ;;
  *) die "SLO_META must be host:port (got '$SLO_META')" ;;
esac

IFS=',' read -r -a NODES <<< "$SLO_NODES"
NODE_COUNT=${#NODES[@]}
for i in "${!NODES[@]}"; do
  node="$(printf '%s' "${NODES[$i]}" | tr -d '[:space:]')"
  [ -n "$node" ] || die "SLO_NODES contains an empty address (got '$SLO_NODES')"
  case "$node" in
    *:*) ;;
    *) die "SLO_NODES entry '$node' must be host:port" ;;
  esac
  NODES[$i]="$node"
done
SLO_NODES="$(IFS=,; printf '%s' "${NODES[*]}")"

if [ "$NODE_COUNT" -lt 3 ] && [ "${SLO_ALLOW_SMALL_CLUSTER:-0}" != "1" ]; then
  die "SLO_NODES has $NODE_COUNT node(s); a multi-host SLO run needs at least 3 (set SLO_ALLOW_SMALL_CLUSTER=1 to override)"
fi
case "$NODE_COUNT" in
  3|5|7) ;;
  *) note "bench-multihost: warning: $NODE_COUNT KV nodes; runbook examples cover 3, 5 and 7" ;;
esac

# ---- defaults -------------------------------------------------------------

export SLO_RUN=1
export SLO_META
export SLO_NODES
export SLO_CLIENTS="${SLO_CLIENTS:-16}"
export SLO_MAX_INFLIGHT="${SLO_MAX_INFLIGHT:-$SLO_CLIENTS}"
export SLO_DURATION="${SLO_DURATION:-10m}"
export SLO_VALUE_SIZE="${SLO_VALUE_SIZE:-1024}"
export SLO_KEYSPACE="${SLO_KEYSPACE:-10000000}"
export SLO_READ_RATIO="${SLO_READ_RATIO:-0.8}"
export SLO_OP_TIMEOUT="${SLO_OP_TIMEOUT:-10s}"
export SLO_VERIFY_RATE="${SLO_VERIFY_RATE:-0.01}"
export SLO_VERIFY_SAMPLE="${SLO_VERIFY_SAMPLE:-32}"
export SLO_PRELOAD_GB="${SLO_PRELOAD_GB:-0}"
export SLO_PRELOAD_WORKERS="${SLO_PRELOAD_WORKERS:-4}"
export SLO_P95_GATE_MS="${SLO_P95_GATE_MS:-0}"
export SLO_P99_GATE_MS="${SLO_P99_GATE_MS:-0}"
export SLO_MAX_ERROR_RATE="${SLO_MAX_ERROR_RATE:-0}"
export SLO_MIN_THROUGHPUT="${SLO_MIN_THROUGHPUT:-0}"
export SLO_TEST_TIMEOUT="${SLO_TEST_TIMEOUT:-24h}"

# ---- validation -----------------------------------------------------------

require_uint SLO_CLIENTS "$SLO_CLIENTS" 1 1024
require_uint SLO_MAX_INFLIGHT "$SLO_MAX_INFLIGHT" 1 8192
require_duration SLO_DURATION "$SLO_DURATION"
require_uint SLO_VALUE_SIZE "$SLO_VALUE_SIZE" 1 16777216
require_uint SLO_KEYSPACE "$SLO_KEYSPACE" 1 1099511627776
awk -v k="$SLO_KEYSPACE" -v c="$SLO_CLIENTS" 'BEGIN { exit !(k + 0 >= c + 0) }' \
  || die "SLO_KEYSPACE=$SLO_KEYSPACE must be >= SLO_CLIENTS=$SLO_CLIENTS"
require_float SLO_READ_RATIO "$SLO_READ_RATIO" 0 1
require_duration SLO_OP_TIMEOUT "$SLO_OP_TIMEOUT"
require_float SLO_VERIFY_RATE "$SLO_VERIFY_RATE" 0 1
require_uint SLO_VERIFY_SAMPLE "$SLO_VERIFY_SAMPLE" 0 10000
require_float SLO_PRELOAD_GB "$SLO_PRELOAD_GB" 0 1048576
require_float SLO_P95_GATE_MS "$SLO_P95_GATE_MS" 0 86400000
require_float SLO_P99_GATE_MS "$SLO_P99_GATE_MS" 0 86400000
require_float SLO_MAX_ERROR_RATE "$SLO_MAX_ERROR_RATE" 0 1
require_float SLO_MIN_THROUGHPUT "$SLO_MIN_THROUGHPUT" 0 1000000000000
require_duration SLO_TEST_TIMEOUT "$SLO_TEST_TIMEOUT"

if [ -n "${SLO_PRELOAD_WORKERS:-}" ]; then
  require_uint SLO_PRELOAD_WORKERS "$SLO_PRELOAD_WORKERS" 1 64
  export SLO_PRELOAD_WORKERS
fi
if [ -n "${SLO_SEED:-}" ]; then
  printf '%s\n' "$SLO_SEED" | grep -Eq '^[0-9]+$' || die "SLO_SEED must be an unsigned integer (got '$SLO_SEED')"
  export SLO_SEED
fi

# ---- outputs --------------------------------------------------------------

RUN_ID="${SLO_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
case "$RUN_ID" in
  *[!A-Za-z0-9._-]*) die "SLO_RUN_ID may contain only letters, digits, '.', '_' and '-'" ;;
esac
OUT_DIR="${SLO_OUT_DIR:-$ROOT_DIR/.tmp-slo}"
case "$OUT_DIR" in
  /*) ;;
  *) OUT_DIR="$ROOT_DIR/$OUT_DIR" ;;
esac
LOG="$OUT_DIR/slo-multihost-$RUN_ID.log"
REPORT="$OUT_DIR/slo-multihost-$RUN_ID.json"
if [ -n "${SLO_REPORT_JSON:-}" ]; then
  case "$SLO_REPORT_JSON" in
    /*) ;;
    *) SLO_REPORT_JSON="$ROOT_DIR/$SLO_REPORT_JSON" ;;
  esac
else
  SLO_REPORT_JSON="$REPORT"
fi
export SLO_REPORT_JSON

print_config() {
  printf '# multi-host SLO run\n'
  printf '# generated_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '# host=%s\n' "$(hostname 2>/dev/null || printf unknown)"
  printf 'SLO_RUN=%s\n' "$SLO_RUN"
  printf 'SLO_META=%s\n' "$SLO_META"
  printf 'SLO_NODES=%s\n' "$SLO_NODES"
  printf 'SLO_CLIENTS=%s\n' "$SLO_CLIENTS"
  printf 'SLO_MAX_INFLIGHT=%s\n' "$SLO_MAX_INFLIGHT"
  printf 'SLO_DURATION=%s\n' "$SLO_DURATION"
  printf 'SLO_VALUE_SIZE=%s\n' "$SLO_VALUE_SIZE"
  printf 'SLO_KEYSPACE=%s\n' "$SLO_KEYSPACE"
  printf 'SLO_READ_RATIO=%s\n' "$SLO_READ_RATIO"
  printf 'SLO_OP_TIMEOUT=%s\n' "$SLO_OP_TIMEOUT"
  printf 'SLO_VERIFY_RATE=%s\n' "$SLO_VERIFY_RATE"
  printf 'SLO_VERIFY_SAMPLE=%s\n' "$SLO_VERIFY_SAMPLE"
  printf 'SLO_PRELOAD_GB=%s\n' "$SLO_PRELOAD_GB"
  printf 'SLO_PRELOAD_WORKERS=%s\n' "$SLO_PRELOAD_WORKERS"
  printf 'SLO_SEED=%s\n' "${SLO_SEED:-}"
  printf 'SLO_P95_GATE_MS=%s\n' "$SLO_P95_GATE_MS"
  printf 'SLO_P99_GATE_MS=%s\n' "$SLO_P99_GATE_MS"
  printf 'SLO_MAX_ERROR_RATE=%s\n' "$SLO_MAX_ERROR_RATE"
  printf 'SLO_MIN_THROUGHPUT=%s\n' "$SLO_MIN_THROUGHPUT"
  printf 'SLO_TEST_TIMEOUT=%s\n' "$SLO_TEST_TIMEOUT"
  printf 'SLO_REPORT_JSON=%s\n' "$SLO_REPORT_JSON"
}

if [ "$DRY_RUN" = "1" ]; then
  print_config
  note "bench-multihost: dry-run OK (cluster not contacted)"
  exit 0
fi

command -v go >/dev/null 2>&1 || die "go not found in PATH (Go 1.23+ is required)"
[ -f "$ROOT_DIR/test/slo_multihost_test.go" ] || die "test/slo_multihost_test.go is missing"

mkdir -p "$OUT_DIR" || die "cannot create output directory $OUT_DIR"
REPORT_DIR="$(dirname -- "$SLO_REPORT_JSON")"
mkdir -p "$REPORT_DIR" || die "cannot create report directory $REPORT_DIR"
if [ -d "$SLO_REPORT_JSON" ]; then
  die "SLO_REPORT_JSON points to a directory: $SLO_REPORT_JSON"
fi
: > "$LOG"

print_config | tee -a "$LOG"
printf '# go_version=%s\n' "$(go version 2>/dev/null || printf unknown)" | tee -a "$LOG"

note "bench-multihost: running TestSLOMultiHost; this may take hours for a large preload"
set +e
(
  cd "$ROOT_DIR"
  go test -run '^TestSLOMultiHost$' -count=1 -v -timeout "$SLO_TEST_TIMEOUT" ./test
) 2>&1 | tee -a "$LOG"
pipeline_status=("${PIPESTATUS[@]}")
rc=${pipeline_status[0]}
tee_rc=${pipeline_status[1]}
set -e

if [ "$tee_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then
  printf 'bench-multihost: FAIL (could not capture output to %s, tee exit %d)\n' "$LOG" "$tee_rc" >&2
  exit 1
fi

if [ "$rc" -ne 0 ]; then
  printf 'bench-multihost: FAIL (go test exit %d)\n' "$rc" | tee -a "$LOG" >&2
  printf 'log: %s\n' "$LOG" >&2
  exit "$rc"
fi

if [ ! -s "$SLO_REPORT_JSON" ]; then
  printf 'bench-multihost: harness error: report is missing or empty: %s\n' "$SLO_REPORT_JSON" | tee -a "$LOG" >&2
  exit 1
fi

printf 'bench-multihost: PASS\n' | tee -a "$LOG"
printf 'log: %s\n' "$LOG" | tee -a "$LOG"
printf 'report: %s\n' "$SLO_REPORT_JSON" | tee -a "$LOG"
