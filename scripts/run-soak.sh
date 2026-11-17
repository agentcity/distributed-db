#!/usr/bin/env bash
#
# run-soak.sh - run the external-cluster soak harness (TestSoakMultiHost).
#
# This script never starts a meta service, a KV node, a container or a
# database. It validates SOAK_* configuration, runs the already-deployed
# cluster test, captures the full output with tee, and exits non-zero when the
# harness or a configured gate fails.
#
# Quick start (3 KV nodes; replace the addresses with your cluster):
#
#   export SOAK_META=10.20.0.10:9090
#   export SOAK_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
#   ./scripts/run-soak.sh --duration 72h --preload-gb 1024 \
#     --p99-ms 250 --memory-growth-mib-per-hour 64 \
#     --metrics-urls http://10.20.0.11:9100/metrics,http://10.20.0.12:9100/metrics,http://10.20.0.13:9100/metrics \
#     --wal-growth-mib-per-hour 128
#
# 5 KV nodes:
#
#   export SOAK_META=10.20.0.10:9090
#   export SOAK_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001
#   ./scripts/run-soak.sh --duration 72h --preload-gb 1024 --clients 24
#
# Outputs:
#   $SOAK_OUT_DIR/soak-<run-id>.log    full test output
#   $SOAK_REPORT_JSON (or $SOAK_OUT_DIR/soak-<run-id>.json)  JSON evidence
#
# Exit codes: 0 = pass, 1 = harness/gate failure, 2 = usage/config error.
#
# See docs/SOAK.md before treating any result as production evidence. This
# repository has not performed a 72 h or 1 TB run; the script is the procedure,
# not proof.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  printf 'run-soak: %s\n' "$*" >&2
  exit 2
}

note() {
  printf '%s\n' "$*"
}

usage() {
  cat <<'EOF'
Usage: scripts/run-soak.sh [options]

Runs the external-cluster test TestSoakMultiHost. The cluster must already be
running; this script never starts servers.

Required (environment or flags):
  --meta HOST:PORT        meta/TSO gRPC address (or SOAK_META)
  --nodes A,B,C           comma-separated KV gRPC host:port list (or SOAK_NODES)
                          3 or 5 nodes recommended; set SOAK_ALLOW_SMALL_CLUSTER=1
                          to override the 3-node minimum.

Core options:
  --duration D            measured duration, default 72h (integer s/m/h/d units)
  --preload-gb N          preload value payload in GiB, default 0 (1024 = 1 TiB)
  --clients N             concurrent client goroutines, default 16 (max 1024)
  --max-inflight N        global in-flight transaction cap, default clients
  --read-ratio F          fraction of reads in the mix, default 0.8
  --value-size N          value payload bytes, default 1024
  --keyspace N            logical key budget, partitioned by client
  --sample-interval D     metrics/drift bucket interval, default 1m
  --metrics-urls URLS     comma-separated Prometheus text endpoints for WAL
                          and compaction evidence (optional, strongly advised)
  --disk-probe 0|1        RangeStats disk probe, default 1
  --p99-ms F              fail when p99 exceeds F ms (0 disables)
  --max-error-rate F      fail above this workload error ratio (default 0.001)
  --memory-growth-mib-per-hour F
                          fail above this client heap slope (0 disables)
  --wal-growth-mib-per-hour F
                          fail above this WAL slope (0 disables; requires metrics)
  --min-throughput F      fail below this aggregate ops/s (0 disables)

Output and control:
  --out-dir DIR           artifact directory (default .tmp-soak under repo root)
  --report PATH           explicit JSON report path
  --timeout D             go test timeout; default derived from duration and
                          preload, plus two hours of slack
  --dry-run               validate and print the effective config; do not run
  --example 3|5           print a copy-paste environment example and exit
  -h, --help              show this help

Environment variables not listed above are forwarded unchanged; see
docs/SOAK.md for the full SOAK_* table. Use absolute paths for SOAK_REPORT_JSON
when invoking go test directly (the test's working directory is ./test).
EOF
}

show_example() {
  case "${1:-3}" in
    3)
      cat <<'EOF'
# 3 KV nodes (RF=3). Replace every address with your cluster.
# A 1 TiB value payload needs roughly 3.3 TiB of raw cluster capacity at RF=3,
# plus WAL/SST headroom; stage that before starting a 72 h run.
export SOAK_META=10.20.0.10:9090
export SOAK_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001
export SOAK_CLIENTS=16
export SOAK_MAX_INFLIGHT=16
export SOAK_DURATION=72h
export SOAK_VALUE_SIZE=1024
export SOAK_KEYSPACE=10000000
export SOAK_READ_RATIO=0.8
export SOAK_PRELOAD_GB=1024
export SOAK_METRICS_URLS=http://10.20.0.11:9100/metrics,http://10.20.0.12:9100/metrics,http://10.20.0.13:9100/metrics
export SOAK_P99_GATE_MS=250
export SOAK_MAX_ERROR_RATE=0.001
export SOAK_MEMORY_GROWTH_MIB_PER_HOUR=64
export SOAK_WAL_GROWTH_MIB_PER_HOUR=128
export SOAK_OUT_DIR=./.tmp-soak
./scripts/run-soak.sh --duration 72h --preload-gb 1024
EOF
      ;;
    5)
      cat <<'EOF'
# 5 KV nodes. Use a topology whose failure domains match production.
# 1 TiB at RF=3 still needs about 3.3 TiB raw; five nodes spread that load.
export SOAK_META=10.20.0.10:9090
export SOAK_NODES=10.20.0.11:7001,10.20.0.12:7001,10.20.0.13:7001,10.20.0.14:7001,10.20.0.15:7001
export SOAK_CLIENTS=24
export SOAK_MAX_INFLIGHT=24
export SOAK_DURATION=72h
export SOAK_VALUE_SIZE=1024
export SOAK_KEYSPACE=20000000
export SOAK_READ_RATIO=0.8
export SOAK_PRELOAD_GB=1024
export SOAK_METRICS_URLS=http://10.20.0.11:9100/metrics,http://10.20.0.12:9100/metrics,http://10.20.0.13:9100/metrics,http://10.20.0.14:9100/metrics,http://10.20.0.15:9100/metrics
export SOAK_P99_GATE_MS=250
export SOAK_MAX_ERROR_RATE=0.001
export SOAK_MEMORY_GROWTH_MIB_PER_HOUR=64
export SOAK_WAL_GROWTH_MIB_PER_HOUR=128
export SOAK_OUT_DIR=./.tmp-soak
./scripts/run-soak.sh --duration 72h --preload-gb 1024
EOF
      ;;
    *)
      die "--example expects 3 or 5 (got '$1')"
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
  if ! printf '%s\n' "$value" | grep -Eq '^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h|d))+$|^[0-9]+([.][0-9]+)?$'; then
    die "$name must be a Go duration such as 30s, 90m or 72h, or seconds (got '$value')"
  fi
}

# duration_seconds converts simple integer-unit durations (72h, 1h30m, 300s)
# to seconds. It is used only to derive a default go test timeout; failure is
# non-fatal because the test itself accepts any Go duration.
duration_seconds() {
  local rest="$1" total=0 num unit
  if [[ "$rest" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "$rest"
    return 0
  fi
  while [[ "$rest" =~ ^([0-9]+)([smhd])(.*)$ ]]; do
    num="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    rest="${BASH_REMATCH[3]}"
    case "$unit" in
      s) total=$((total + num)) ;;
      m) total=$((total + num * 60)) ;;
      h) total=$((total + num * 3600)) ;;
      d) total=$((total + num * 86400)) ;;
    esac
  done
  if [ -n "$rest" ]; then
    return 1
  fi
  printf '%s\n' "$total"
}

DRY_RUN=0
DURATION_ARG=""
PRELOAD_ARG=""
REPORT_ARG=""
OUT_DIR_ARG=""
TIMEOUT_ARG=""
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
      [ "$#" -ge 2 ] || die "--example requires 3 or 5"
      show_example "$2"
      exit 0
      ;;
    --example=*)
      show_example "${1#*=}"
      exit 0
      ;;
    --duration)
      [ "$#" -ge 2 ] || die "--duration requires a value"
      DURATION_ARG="$2"
      shift 2
      ;;
    --duration=*)
      DURATION_ARG="${1#*=}"
      shift
      ;;
    --preload-gb)
      [ "$#" -ge 2 ] || die "--preload-gb requires a value"
      PRELOAD_ARG="$2"
      shift 2
      ;;
    --preload-gb=*)
      PRELOAD_ARG="${1#*=}"
      shift
      ;;
    --meta)
      [ "$#" -ge 2 ] || die "--meta requires host:port"
      SOAK_META="$2"
      shift 2
      ;;
    --meta=*)
      SOAK_META="${1#*=}"
      shift
      ;;
    --nodes)
      [ "$#" -ge 2 ] || die "--nodes requires a comma-separated list"
      SOAK_NODES="$2"
      shift 2
      ;;
    --nodes=*)
      SOAK_NODES="${1#*=}"
      shift
      ;;
    --clients)
      [ "$#" -ge 2 ] || die "--clients requires a value"
      SOAK_CLIENTS="$2"
      shift 2
      ;;
    --clients=*)
      SOAK_CLIENTS="${1#*=}"
      shift
      ;;
    --max-inflight)
      [ "$#" -ge 2 ] || die "--max-inflight requires a value"
      SOAK_MAX_INFLIGHT="$2"
      shift 2
      ;;
    --max-inflight=*)
      SOAK_MAX_INFLIGHT="${1#*=}"
      shift
      ;;
    --read-ratio)
      [ "$#" -ge 2 ] || die "--read-ratio requires a value"
      SOAK_READ_RATIO="$2"
      shift 2
      ;;
    --read-ratio=*)
      SOAK_READ_RATIO="${1#*=}"
      shift
      ;;
    --value-size)
      [ "$#" -ge 2 ] || die "--value-size requires a value"
      SOAK_VALUE_SIZE="$2"
      shift 2
      ;;
    --value-size=*)
      SOAK_VALUE_SIZE="${1#*=}"
      shift
      ;;
    --keyspace)
      [ "$#" -ge 2 ] || die "--keyspace requires a value"
      SOAK_KEYSPACE="$2"
      shift 2
      ;;
    --keyspace=*)
      SOAK_KEYSPACE="${1#*=}"
      shift
      ;;
    --sample-interval)
      [ "$#" -ge 2 ] || die "--sample-interval requires a value"
      SOAK_SAMPLE_INTERVAL="$2"
      shift 2
      ;;
    --sample-interval=*)
      SOAK_SAMPLE_INTERVAL="${1#*=}"
      shift
      ;;
    --metrics-urls)
      [ "$#" -ge 2 ] || die "--metrics-urls requires a comma-separated list"
      SOAK_METRICS_URLS="$2"
      shift 2
      ;;
    --metrics-urls=*)
      SOAK_METRICS_URLS="${1#*=}"
      shift
      ;;
    --disk-probe)
      [ "$#" -ge 2 ] || die "--disk-probe requires 0 or 1"
      SOAK_DISK_PROBE="$2"
      shift 2
      ;;
    --disk-probe=*)
      SOAK_DISK_PROBE="${1#*=}"
      shift
      ;;
    --p99-ms)
      [ "$#" -ge 2 ] || die "--p99-ms requires a value"
      SOAK_P99_GATE_MS="$2"
      shift 2
      ;;
    --p99-ms=*)
      SOAK_P99_GATE_MS="${1#*=}"
      shift
      ;;
    --max-error-rate)
      [ "$#" -ge 2 ] || die "--max-error-rate requires a value"
      SOAK_MAX_ERROR_RATE="$2"
      shift 2
      ;;
    --max-error-rate=*)
      SOAK_MAX_ERROR_RATE="${1#*=}"
      shift
      ;;
    --memory-growth-mib-per-hour)
      [ "$#" -ge 2 ] || die "--memory-growth-mib-per-hour requires a value"
      SOAK_MEMORY_GROWTH_MIB_PER_HOUR="$2"
      shift 2
      ;;
    --memory-growth-mib-per-hour=*)
      SOAK_MEMORY_GROWTH_MIB_PER_HOUR="${1#*=}"
      shift
      ;;
    --wal-growth-mib-per-hour)
      [ "$#" -ge 2 ] || die "--wal-growth-mib-per-hour requires a value"
      SOAK_WAL_GROWTH_MIB_PER_HOUR="$2"
      shift 2
      ;;
    --wal-growth-mib-per-hour=*)
      SOAK_WAL_GROWTH_MIB_PER_HOUR="${1#*=}"
      shift
      ;;
    --min-throughput)
      [ "$#" -ge 2 ] || die "--min-throughput requires a value"
      SOAK_MIN_THROUGHPUT="$2"
      shift 2
      ;;
    --min-throughput=*)
      SOAK_MIN_THROUGHPUT="${1#*=}"
      shift
      ;;
    --report)
      [ "$#" -ge 2 ] || die "--report requires a path"
      REPORT_ARG="$2"
      shift 2
      ;;
    --report=*)
      REPORT_ARG="${1#*=}"
      shift
      ;;
    --out-dir)
      [ "$#" -ge 2 ] || die "--out-dir requires a path"
      OUT_DIR_ARG="$2"
      shift 2
      ;;
    --out-dir=*)
      OUT_DIR_ARG="${1#*=}"
      shift
      ;;
    --timeout)
      [ "$#" -ge 2 ] || die "--timeout requires a duration"
      TIMEOUT_ARG="$2"
      shift 2
      ;;
    --timeout=*)
      TIMEOUT_ARG="${1#*=}"
      shift
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
done

# ---- required environment -------------------------------------------------

[ -n "${SOAK_META:-}" ] || die "SOAK_META is required (meta/TSO gRPC host:port; try --example 3)"
[ -n "${SOAK_NODES:-}" ] || die "SOAK_NODES is required (comma-separated KV gRPC host:port list; try --example 3)"

case "$SOAK_META" in
  *:*) ;;
  *) die "SOAK_META must be host:port (got '$SOAK_META')" ;;
esac

IFS=',' read -r -a NODES <<< "$SOAK_NODES"
NODE_COUNT=${#NODES[@]}
for i in "${!NODES[@]}"; do
  node="$(printf '%s' "${NODES[$i]}" | tr -d '[:space:]')"
  [ -n "$node" ] || die "SOAK_NODES contains an empty address (got '$SOAK_NODES')"
  case "$node" in
    *:*) ;;
    *) die "SOAK_NODES entry '$node' must be host:port" ;;
  esac
  NODES[$i]="$node"
done
SOAK_NODES="$(IFS=,; printf '%s' "${NODES[*]}")"

if [ "$NODE_COUNT" -lt 3 ] && [ "${SOAK_ALLOW_SMALL_CLUSTER:-0}" != "1" ]; then
  die "SOAK_NODES has $NODE_COUNT node(s); a multi-host soak needs at least 3 (set SOAK_ALLOW_SMALL_CLUSTER=1 to override)"
fi
case "$NODE_COUNT" in
  3|5) ;;
  *) note "run-soak: warning: $NODE_COUNT KV nodes; runbook examples cover 3 and 5" ;;
esac

if [ -n "$DURATION_ARG" ]; then
  SOAK_DURATION="$DURATION_ARG"
fi
if [ -n "$PRELOAD_ARG" ]; then
  SOAK_PRELOAD_GB="$PRELOAD_ARG"
fi
if [ -n "$TIMEOUT_ARG" ]; then
  SOAK_TEST_TIMEOUT="$TIMEOUT_ARG"
fi

# ---- defaults -------------------------------------------------------------

export SOAK_RUN=1
export SOAK_META
export SOAK_NODES
export SOAK_DURATION="${SOAK_DURATION:-72h}"
export SOAK_CLIENTS="${SOAK_CLIENTS:-16}"
export SOAK_MAX_INFLIGHT="${SOAK_MAX_INFLIGHT:-$SOAK_CLIENTS}"
export SOAK_VALUE_SIZE="${SOAK_VALUE_SIZE:-1024}"
export SOAK_KEYSPACE="${SOAK_KEYSPACE:-10000000}"
export SOAK_READ_RATIO="${SOAK_READ_RATIO:-0.8}"
export SOAK_SAMPLE_INTERVAL="${SOAK_SAMPLE_INTERVAL:-1m}"
export SOAK_PRELOAD_GB="${SOAK_PRELOAD_GB:-0}"
export SOAK_MAX_ERROR_RATE="${SOAK_MAX_ERROR_RATE:-0.001}"
export SOAK_P99_GATE_MS="${SOAK_P99_GATE_MS:-0}"
export SOAK_MEMORY_GROWTH_MIB_PER_HOUR="${SOAK_MEMORY_GROWTH_MIB_PER_HOUR:-0}"
export SOAK_WAL_GROWTH_MIB_PER_HOUR="${SOAK_WAL_GROWTH_MIB_PER_HOUR:-0}"
export SOAK_MIN_THROUGHPUT="${SOAK_MIN_THROUGHPUT:-0}"
export SOAK_DISK_PROBE="${SOAK_DISK_PROBE:-1}"
if [ -n "${SOAK_DISK_PROBE_INTERVAL:-}" ]; then
  export SOAK_DISK_PROBE_INTERVAL
fi
if [ -n "${SOAK_METRICS_URLS:-}" ]; then
  export SOAK_METRICS_URLS
fi

# ---- validation -----------------------------------------------------------

require_duration SOAK_DURATION "$SOAK_DURATION"
require_uint SOAK_CLIENTS "$SOAK_CLIENTS" 1 1024
require_uint SOAK_MAX_INFLIGHT "$SOAK_MAX_INFLIGHT" 1 8192
require_uint SOAK_VALUE_SIZE "$SOAK_VALUE_SIZE" 1 16777216
require_uint SOAK_KEYSPACE "$SOAK_KEYSPACE" 1 1099511627776
awk -v k="$SOAK_KEYSPACE" -v c="$SOAK_CLIENTS" 'BEGIN { exit !(k + 0 >= c + 0) }' \
  || die "SOAK_KEYSPACE=$SOAK_KEYSPACE must be >= SOAK_CLIENTS=$SOAK_CLIENTS"
require_float SOAK_READ_RATIO "$SOAK_READ_RATIO" 0 1
require_duration SOAK_SAMPLE_INTERVAL "$SOAK_SAMPLE_INTERVAL"
require_float SOAK_PRELOAD_GB "$SOAK_PRELOAD_GB" 0 1048576
require_float SOAK_MAX_ERROR_RATE "$SOAK_MAX_ERROR_RATE" 0 1
require_float SOAK_P99_GATE_MS "$SOAK_P99_GATE_MS" 0 86400000
require_float SOAK_MEMORY_GROWTH_MIB_PER_HOUR "$SOAK_MEMORY_GROWTH_MIB_PER_HOUR" 0 1000000000
require_float SOAK_WAL_GROWTH_MIB_PER_HOUR "$SOAK_WAL_GROWTH_MIB_PER_HOUR" 0 1000000000
require_float SOAK_MIN_THROUGHPUT "$SOAK_MIN_THROUGHPUT" 0 1000000000000
case "$SOAK_DISK_PROBE" in
  0|1) ;;
  *) die "SOAK_DISK_PROBE must be 0 or 1 (got '$SOAK_DISK_PROBE')" ;;
esac
if [ -n "${SOAK_DISK_PROBE_INTERVAL:-}" ]; then
  require_duration SOAK_DISK_PROBE_INTERVAL "$SOAK_DISK_PROBE_INTERVAL"
fi
if [ -n "${SOAK_METRICS_URLS:-}" ]; then
  IFS=',' read -r -a METRICS_URLS <<< "$SOAK_METRICS_URLS"
  for metrics_url in "${METRICS_URLS[@]}"; do
    case "$metrics_url" in
      http://*|https://*) ;;
      *) die "SOAK_METRICS_URLS entry '$metrics_url' must start with http:// or https://" ;;
    esac
  done
fi
if [ -n "${SOAK_SEED:-}" ]; then
  printf '%s\n' "$SOAK_SEED" | grep -Eq '^[0-9]+$' || die "SOAK_SEED must be an unsigned integer (got '$SOAK_SEED')"
  export SOAK_SEED
fi

# ---- test timeout ---------------------------------------------------------

if [ -z "${SOAK_TEST_TIMEOUT:-}" ]; then
  if DURATION_SECONDS="$(duration_seconds "$SOAK_DURATION")"; then
    PRELOAD_SECONDS="$(awk -v g="$SOAK_PRELOAD_GB" 'BEGIN { printf "%d", (g * 1024) / 10 }')"
    SOAK_TEST_TIMEOUT="$((DURATION_SECONDS + PRELOAD_SECONDS + 7200))s"
  else
    note "run-soak: warning: cannot derive a timeout from SOAK_DURATION=$SOAK_DURATION; using 168h (set --timeout to override)"
    SOAK_TEST_TIMEOUT="168h"
  fi
fi
require_duration SOAK_TEST_TIMEOUT "$SOAK_TEST_TIMEOUT"
export SOAK_TEST_TIMEOUT

# ---- outputs --------------------------------------------------------------

RUN_ID="${SOAK_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
case "$RUN_ID" in
  *[!A-Za-z0-9._-]*) die "SOAK_RUN_ID may contain only letters, digits, '.', '_' and '-'" ;;
esac

if [ -n "$OUT_DIR_ARG" ]; then
  OUT_DIR="$OUT_DIR_ARG"
elif [ -n "${SOAK_OUT_DIR:-}" ]; then
  OUT_DIR="$SOAK_OUT_DIR"
else
  OUT_DIR="$ROOT_DIR/.tmp-soak"
fi
case "$OUT_DIR" in
  /*) ;;
  *) OUT_DIR="$ROOT_DIR/$OUT_DIR" ;;
esac
LOG="$OUT_DIR/soak-$RUN_ID.log"

if [ -n "$REPORT_ARG" ]; then
  SOAK_REPORT_JSON="$REPORT_ARG"
elif [ -n "${SOAK_REPORT_JSON:-}" ]; then
  : # keep the caller's path
else
  SOAK_REPORT_JSON="$OUT_DIR/soak-$RUN_ID.json"
fi
case "$SOAK_REPORT_JSON" in
  /*) ;;
  *) SOAK_REPORT_JSON="$ROOT_DIR/$SOAK_REPORT_JSON" ;;
esac
export SOAK_REPORT_JSON

redact_metrics_urls() {
  printf '%s' "${SOAK_METRICS_URLS:-}" | sed -E 's#(https?://)[^/@]+@#\1***@#g'
}

print_config() {
  printf '# external-cluster soak run\n'
  printf '# generated_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '# host=%s\n' "$(hostname 2>/dev/null || printf unknown)"
  printf '# note=this script does not start any server\n'
  printf 'SOAK_RUN=%s\n' "$SOAK_RUN"
  printf 'SOAK_META=%s\n' "$SOAK_META"
  printf 'SOAK_NODES=%s\n' "$SOAK_NODES"
  printf 'SOAK_DURATION=%s\n' "$SOAK_DURATION"
  printf 'SOAK_CLIENTS=%s\n' "$SOAK_CLIENTS"
  printf 'SOAK_MAX_INFLIGHT=%s\n' "$SOAK_MAX_INFLIGHT"
  printf 'SOAK_VALUE_SIZE=%s\n' "$SOAK_VALUE_SIZE"
  printf 'SOAK_KEYSPACE=%s\n' "$SOAK_KEYSPACE"
  printf 'SOAK_READ_RATIO=%s\n' "$SOAK_READ_RATIO"
  printf 'SOAK_SAMPLE_INTERVAL=%s\n' "$SOAK_SAMPLE_INTERVAL"
  printf 'SOAK_PRELOAD_GB=%s\n' "$SOAK_PRELOAD_GB"
  printf 'SOAK_P99_GATE_MS=%s\n' "$SOAK_P99_GATE_MS"
  printf 'SOAK_MAX_ERROR_RATE=%s\n' "$SOAK_MAX_ERROR_RATE"
  printf 'SOAK_MEMORY_GROWTH_MIB_PER_HOUR=%s\n' "$SOAK_MEMORY_GROWTH_MIB_PER_HOUR"
  printf 'SOAK_WAL_GROWTH_MIB_PER_HOUR=%s\n' "$SOAK_WAL_GROWTH_MIB_PER_HOUR"
  printf 'SOAK_MIN_THROUGHPUT=%s\n' "$SOAK_MIN_THROUGHPUT"
  printf 'SOAK_DISK_PROBE=%s\n' "$SOAK_DISK_PROBE"
  printf 'SOAK_DISK_PROBE_INTERVAL=%s\n' "${SOAK_DISK_PROBE_INTERVAL:-5m}"
  printf 'SOAK_METRICS_URLS=%s\n' "$(redact_metrics_urls)"
  printf 'SOAK_SEED=%s\n' "${SOAK_SEED:-}"
  printf 'SOAK_TEST_TIMEOUT=%s\n' "$SOAK_TEST_TIMEOUT"
  printf 'SOAK_REPORT_JSON=%s\n' "$SOAK_REPORT_JSON"
  printf 'SOAK_LOG=%s\n' "$LOG"
}

if [ "$DRY_RUN" = "1" ]; then
  print_config
  note "run-soak: dry-run OK (cluster not contacted)"
  exit 0
fi

command -v "${GO:-go}" >/dev/null 2>&1 || die "Go executable '${GO:-go}' not found in PATH (Go 1.23+ is required)"
[ -f "$ROOT_DIR/test/soak_test.go" ] || die "test/soak_test.go is missing"

mkdir -p "$OUT_DIR" || die "cannot create output directory $OUT_DIR"
REPORT_DIR="$(dirname -- "$SOAK_REPORT_JSON")"
mkdir -p "$REPORT_DIR" || die "cannot create report directory $REPORT_DIR"
if [ -d "$SOAK_REPORT_JSON" ]; then
  die "SOAK_REPORT_JSON points to a directory: $SOAK_REPORT_JSON"
fi
: > "$LOG"

print_config | tee -a "$LOG"
printf '# go_version=%s\n' "$("${GO:-go}" version 2>/dev/null || printf unknown)" | tee -a "$LOG"

if awk -v g="$SOAK_PRELOAD_GB" 'BEGIN { exit !(g + 0 >= 100) }'; then
  note "run-soak: warning: large preload; the default timeout assumes 10 MiB/s. Measure a 10-50 GiB pilot and pass --timeout if this cluster is slower."
fi

note "run-soak: running TestSoakMultiHost; a 1 TiB preload plus 72 h measured phase can take days"
set +e
(
  cd "$ROOT_DIR"
  "${GO:-go}" test -run '^TestSoakMultiHost$' -count=1 -v -timeout "$SOAK_TEST_TIMEOUT" ./test
) 2>&1 | tee -a "$LOG"
pipeline_status=("${PIPESTATUS[@]}")
rc=${pipeline_status[0]:-1}
tee_rc=${pipeline_status[1]:-0}
set -e

if [ "$tee_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then
  printf 'run-soak: FAIL (could not capture output to %s, tee exit %d)\n' "$LOG" "$tee_rc" >&2
  exit 1
fi

if [ "$rc" -ne 0 ]; then
  printf 'run-soak: FAIL (go test exit %d)\n' "$rc" | tee -a "$LOG" >&2
  printf 'log: %s\n' "$LOG" >&2
  if [ -s "$SOAK_REPORT_JSON" ]; then
    printf 'report: %s\n' "$SOAK_REPORT_JSON" >&2
  fi
  exit "$rc"
fi

if [ ! -s "$SOAK_REPORT_JSON" ]; then
  printf 'run-soak: harness error: report is missing or empty: %s\n' "$SOAK_REPORT_JSON" | tee -a "$LOG" >&2
  exit 1
fi

printf 'run-soak: PASS\n' | tee -a "$LOG"
printf 'log: %s\n' "$LOG" | tee -a "$LOG"
printf 'report: %s\n' "$SOAK_REPORT_JSON" | tee -a "$LOG"
