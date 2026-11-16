#!/usr/bin/env bash
# Nightly chaos/soak runner for distributed-db.
#
# Runs the opt-in Jepsen partition/leader/failover scenarios plus the v2
# snapshot and split integration tests under the race detector. Each round uses
# a fresh random JEPSEN_SEED (the tests' supported seed input), and every round
# is logged separately. Logs, metadata and a CSV/Markdown summary are written
# under artifacts/chaos/<UTC timestamp>/.
#
# Usage:
#   ./scripts/nightly-chaos.sh                    # 3 rounds, 20 min/round
#   CHAOS_ROUNDS=1 ./scripts/nightly-chaos.sh     # quick local smoke round
#   CHAOS_ROUNDS=1 CHAOS_SHORT=1 ./scripts/nightly-chaos.sh
#
# Environment:
#   CHAOS_ROUNDS              number of rounds (default: 3)
#   CHAOS_TIMEOUT             per-round timeout in seconds (default: 1200)
#   GOFLAGS                   forwarded to go test (default: inherited/empty)
#   CHAOS_TEST_PATTERN        -run regex (default: the documented chaos set)
#   CHAOS_PKG                 package pattern (default: ./test)
#   CHAOS_ARTIFACTS_DIR       artifact root (default: artifacts/chaos)
#   CHAOS_SHORT               1 => add -short to go test (default: 0)
#   CHAOS_JEPSEN              1 => export JEPSEN=1 (default: 1)
#   CHAOS_GODEBUG             GODEBUG for the test process (default:
#                             randautoseed=1; inherited GODEBUG wins if set)
#   CHAOS_SHUFFLE             1 => add -shuffle=on (default: 0)
#   CHAOS_CONTINUE_ON_FAILURE 1 => run every round even after a failure
#                             (default: 0, fail fast)
#   CHAOS_TMPDIR              parent for the per-run scratch directory
#                             (default: $TMPDIR, then /tmp)
#   CHAOS_TIMESTAMP           override the UTC artifact directory timestamp
#
# Exit status: 0 all rounds passed, 1 a round failed/timed out, 2 bad config.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/nightly-chaos.sh [--short]

Runs the nightly chaos test rounds and writes artifacts/chaos/<timestamp>/.

Options:
  --short   add -short to go test (integration tests skip; quick local check)
  -h, --help

Configure with CHAOS_ROUNDS, CHAOS_TIMEOUT and GOFLAGS (see script header).
EOF
}

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GO="${GO:-go}"
CHAOS_ROUNDS="${CHAOS_ROUNDS:-3}"
CHAOS_TIMEOUT="${CHAOS_TIMEOUT:-1200}"
GOFLAGS="${GOFLAGS:-}"
CHAOS_TEST_PATTERN="${CHAOS_TEST_PATTERN:-TestJepsen|TestV2.*Failover|TestV2.*Snapshot|TestV2.*Split}"
CHAOS_PKG="${CHAOS_PKG:-./test}"
CHAOS_ARTIFACTS_DIR="${CHAOS_ARTIFACTS_DIR:-$ROOT_DIR/artifacts/chaos}"
CHAOS_SHORT="${CHAOS_SHORT:-0}"
CHAOS_JEPSEN="${CHAOS_JEPSEN:-1}"
CHAOS_SHUFFLE="${CHAOS_SHUFFLE:-0}"
CHAOS_CONTINUE_ON_FAILURE="${CHAOS_CONTINUE_ON_FAILURE:-0}"
CHAOS_GODEBUG="${CHAOS_GODEBUG:-${GODEBUG:-randautoseed=1}}"
CHAOS_TMPDIR="${CHAOS_TMPDIR:-${TMPDIR:-/tmp}}"

while (($# > 0)); do
  case "$1" in
    --short) CHAOS_SHORT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "nightly-chaos: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

require_uint() {
  local name=$1 value=$2
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "nightly-chaos: $name must be a positive integer, got '$value'" >&2
    exit 2
  fi
}

require_flag() {
  local name=$1 value=$2
  if [[ "$value" != "0" && "$value" != "1" ]]; then
    echo "nightly-chaos: $name must be 0 or 1, got '$value'" >&2
    exit 2
  fi
}

require_uint CHAOS_ROUNDS "$CHAOS_ROUNDS"
require_uint CHAOS_TIMEOUT "$CHAOS_TIMEOUT"
require_flag CHAOS_SHORT "$CHAOS_SHORT"
require_flag CHAOS_JEPSEN "$CHAOS_JEPSEN"
require_flag CHAOS_SHUFFLE "$CHAOS_SHUFFLE"
require_flag CHAOS_CONTINUE_ON_FAILURE "$CHAOS_CONTINUE_ON_FAILURE"

if [[ -z "$CHAOS_TEST_PATTERN" ]]; then
  echo "nightly-chaos: CHAOS_TEST_PATTERN must not be empty" >&2
  exit 2
fi

if ! command -v "$GO" >/dev/null 2>&1; then
  echo "nightly-chaos: Go executable '$GO' not found in PATH" >&2
  exit 2
fi

export GOFLAGS

STAMP="${CHAOS_TIMESTAMP:-$(date -u +%Y%m%dT%H%M%SZ)}"
if [[ ! "$STAMP" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "nightly-chaos: invalid CHAOS_TIMESTAMP '$STAMP'" >&2
  exit 2
fi

RUN_DIR="$CHAOS_ARTIFACTS_DIR/$STAMP"
suffix=1
while [[ -e "$RUN_DIR" ]]; do
  RUN_DIR="$CHAOS_ARTIFACTS_DIR/${STAMP}-${suffix}"
  suffix=$((suffix + 1))
done
mkdir -p "$RUN_DIR"

SUMMARY_CSV="$RUN_DIR/summary.csv"
SUMMARY_MD="$RUN_DIR/summary.md"
SUMMARY_TXT="$RUN_DIR/summary.txt"
METADATA="$RUN_DIR/metadata.txt"

TMP_DIR="$(mktemp -d "${CHAOS_TMPDIR%/}/distributed-db-chaos.XXXXXX")"
export TMPDIR="$TMP_DIR"
export GOTMPDIR="${GOTMPDIR:-$TMP_DIR/gotmp}"
mkdir -p "$GOTMPDIR"

CURRENT_PID=""
TAIL_PID=""
FAILURES=0
COMPLETED=0
FINALIZED=0

# terminate_process_group signals every process in the round's process group.
# A round runs as a background job with job control enabled (set -m), so it owns
# a process group: killing only the `go test` process would leave the test
# binary (and any helpers it spawned) running.
terminate_process_group() {
  local sig=$1
  [[ -n "$CURRENT_PID" ]] || return 0
  kill -"$sig" -- "-$CURRENT_PID" 2>/dev/null || kill -"$sig" "$CURRENT_PID" 2>/dev/null || true
}

# process_group_alive reports whether any member of the round's process group
# is still running. Checking the leader alone would miss a test binary that
# outlived a crashed/killed `go test` parent.
process_group_alive() {
  [[ -n "$CURRENT_PID" ]] || return 1
  kill -0 -- "-$CURRENT_PID" 2>/dev/null || kill -0 "$CURRENT_PID" 2>/dev/null
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -n "$CURRENT_PID" ]]; then
    terminate_process_group TERM
    local grace=0
    while process_group_alive && (( grace < 5 )); do
      sleep 1
      grace=$((grace + 1))
    done
    terminate_process_group KILL
    wait "$CURRENT_PID" 2>/dev/null || true
    CURRENT_PID=""
  fi
  if [[ -n "$TAIL_PID" ]]; then
    kill -TERM "$TAIL_PID" 2>/dev/null || true
    wait "$TAIL_PID" 2>/dev/null || true
    TAIL_PID=""
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
  if [[ "$FINALIZED" -eq 0 && -d "$RUN_DIR" ]]; then
    {
      printf '\n**Run interrupted or terminated before completion.**\n'
      printf '\nCompleted rounds: %d/%d. Artifacts are partial.\n' "$COMPLETED" "$CHAOS_ROUNDS"
    } >>"$SUMMARY_MD" 2>/dev/null || true
  fi
  return "$rc"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

rel_path() {
  local path=$1
  case "$path" in
    "$ROOT_DIR"/*) printf '%s' "${path#"$ROOT_DIR"/}" ;;
    *) printf '%s' "$path" ;;
  esac
}

# run_with_timeout supervises one round in its own process group and returns
# the round's exit status, or 124 after killing a timed-out process group.
run_with_timeout() {
  local limit=$1
  shift

  # A dedicated process group makes timeout and interrupt cleanup reliable on
  # both GNU/Linux (CI) and macOS, where timeout(1) is not installed by default.
  set -m
  "$@" &
  CURRENT_PID=$!
  set +m

  local deadline=$(( $(date +%s) + limit ))
  while process_group_alive; do
    if (( $(date +%s) >= deadline )); then
      echo "nightly-chaos: round exceeded ${limit}s; terminating process group ${CURRENT_PID}" >&2
      terminate_process_group TERM
      local grace=0
      while process_group_alive && (( grace < 5 )); do
        sleep 1
        grace=$((grace + 1))
      done
      terminate_process_group KILL
      wait "$CURRENT_PID" 2>/dev/null || true
      CURRENT_PID=""
      return 124
    fi
    sleep 1
  done

  local rc=0
  wait "$CURRENT_PID" || rc=$?
  CURRENT_PID=""
  return "$rc"
}

count_matches() {
  local pattern=$1 file=$2
  grep -c -E "$pattern" "$file" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Metadata and summary header
# ---------------------------------------------------------------------------

{
  echo "run_id=$STAMP"
  echo "started_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "root_dir=$ROOT_DIR"
  echo "rounds=$CHAOS_ROUNDS"
  echo "timeout_seconds=$CHAOS_TIMEOUT"
  echo "test_pattern=$CHAOS_TEST_PATTERN"
  echo "package=$CHAOS_PKG"
  echo "go=$GO"
  echo "go_version=$("$GO" version 2>&1 || true)"
  echo "goflags=${GOFLAGS:-<empty>}"
  echo "short=$CHAOS_SHORT"
  echo "jepsen=$CHAOS_JEPSEN"
  echo "shuffle=$CHAOS_SHUFFLE"
  echo "godebug=$CHAOS_GODEBUG"
  echo "continue_on_failure=$CHAOS_CONTINUE_ON_FAILURE"
  echo "artifacts_dir=$RUN_DIR"
  echo "host=$(uname -srm 2>/dev/null || true)"
  echo "cwd=$(pwd)"
} >"$METADATA" 2>&1

{
  echo "# Nightly chaos run $STAMP"
  echo
  echo "- Started (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "- Rounds: $CHAOS_ROUNDS"
  echo "- Per-round timeout: ${CHAOS_TIMEOUT}s"
  echo "- Package: \`$CHAOS_PKG\`"
  echo "- Test pattern: \`$CHAOS_TEST_PATTERN\`"
  if [[ "$CHAOS_JEPSEN" == "1" ]]; then
    echo "- JEPSEN: enabled; seed varies per round via JEPSEN_SEED"
  else
    echo "- JEPSEN: disabled; seed env is still varied per round"
  fi
  echo "- GOFLAGS: \`${GOFLAGS:-<empty>}\`"
  echo "- GODEBUG: \`${CHAOS_GODEBUG:-<empty>}\`"
  echo
  echo "| Round | Seed | Status | Duration | Passed | Failed | Skipped | Log |"
  echo "|---:|---:|---|---:|---:|---:|---:|---|"
} >"$SUMMARY_MD"

printf 'round,seed,status,exit_code,duration_s,passed,failed,skipped,log\n' >"$SUMMARY_CSV"

# ---------------------------------------------------------------------------
# Rounds
# ---------------------------------------------------------------------------

for ((round = 1; round <= CHAOS_ROUNDS; round++)); do
  seed=$(( (RANDOM << 16) ^ RANDOM ^ (round * 2654435761) ^ ($(date +%s) & 0x7fffffff) ))
  seed=$(( seed & 0x7fffffff ))
  if (( seed == 0 )); then
    seed=1
  fi

  log="$RUN_DIR/round-$(printf '%02d' "$round").log"
  : >"$log"

  cmd=("$GO" test -race -count=1 -v -timeout "${CHAOS_TIMEOUT}s")
  if [[ "$CHAOS_SHORT" == "1" ]]; then
    cmd+=(-short)
  fi
  if [[ "$CHAOS_SHUFFLE" == "1" ]]; then
    cmd+=(-shuffle=on)
  fi
  cmd+=(-run "$CHAOS_TEST_PATTERN" "$CHAOS_PKG")

  env_args=("JEPSEN_SEED=$seed" "TZ=UTC" "CHAOS_ROUND=$round")
  if [[ "$CHAOS_JEPSEN" == "1" ]]; then
    env_args+=("JEPSEN=1")
  fi
  if [[ -n "$CHAOS_GODEBUG" ]]; then
    env_args+=("GODEBUG=$CHAOS_GODEBUG")
  fi

  {
    printf 'round=%d seed=%d started_at_utc=%s\n' "$round" "$seed" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'timeout=%ss\n' "$CHAOS_TIMEOUT"
    printf 'command:'
    printf ' %q' env "${env_args[@]}" "${cmd[@]}"
    printf '\n\n'
  } >>"$log"

  echo "nightly-chaos: round $round/$CHAOS_ROUNDS (seed=$seed), log: $(rel_path "$log")"

  tail -n +1 -f "$log" &
  TAIL_PID=$!

  round_start=$(date +%s)
  outer_timeout=$((CHAOS_TIMEOUT + 60))
  set +e
  run_with_timeout "$outer_timeout" env "${env_args[@]}" "${cmd[@]}" >>"$log" 2>&1
  status=$?
  set -e

  # Give tail a moment to flush the final lines before stopping it.
  sleep 0.2
  kill -TERM "$TAIL_PID" 2>/dev/null || true
  wait "$TAIL_PID" 2>/dev/null || true
  TAIL_PID=""

  duration=$(( $(date +%s) - round_start ))
  passed=$(count_matches '^[[:space:]]*--- PASS:' "$log")
  failed=$(count_matches '^[[:space:]]*--- FAIL:' "$log")
  skipped=$(count_matches '^[[:space:]]*--- SKIP:' "$log")

  if (( status == 0 )); then
    round_status=PASS
  elif (( status == 124 )) || grep -q -E 'panic: test timed out|test timed out after' "$log"; then
    round_status=TIMEOUT
  else
    round_status=FAIL
  fi

  COMPLETED=$round
  if [[ "$round_status" != "PASS" ]]; then
    FAILURES=$((FAILURES + 1))
  fi

  rel_log=$(rel_path "$log")
  printf '%d,%d,%s,%d,%d,%d,%d,%d,%s\n' \
    "$round" "$seed" "$round_status" "$status" "$duration" "$passed" "$failed" "$skipped" "$rel_log" \
    >>"$SUMMARY_CSV"
  printf '| %d | %d | %s | %ds | %d | %d | %d | `%s` |\n' \
    "$round" "$seed" "$round_status" "$duration" "$passed" "$failed" "$skipped" "$rel_log" \
    >>"$SUMMARY_MD"

  echo "nightly-chaos: round $round: $round_status (exit=$status, ${duration}s, pass=$passed fail=$failed skip=$skipped)"

  if [[ "$round_status" != "PASS" && "$CHAOS_CONTINUE_ON_FAILURE" != "1" ]]; then
    echo "nightly-chaos: stopping after failed round (set CHAOS_CONTINUE_ON_FAILURE=1 to run all rounds)" >&2
    break
  fi
done

# ---------------------------------------------------------------------------
# Final summary
# ---------------------------------------------------------------------------

if (( FAILURES == 0 && COMPLETED == CHAOS_ROUNDS )); then
  OVERALL=PASS
else
  OVERALL=FAIL
fi

{
  echo
  echo "## Result"
  echo
  echo "- Overall: **$OVERALL**"
  echo "- Completed rounds: $COMPLETED/$CHAOS_ROUNDS"
  echo "- Failed rounds: $FAILURES"
  echo "- Finished (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "- Artifacts: \`$(rel_path "$RUN_DIR")\`"
} >>"$SUMMARY_MD"

{
  echo "status=$OVERALL"
  echo "rounds_requested=$CHAOS_ROUNDS"
  echo "rounds_completed=$COMPLETED"
  echo "rounds_failed=$FAILURES"
  echo "started_at_utc=$(grep '^started_at_utc=' "$METADATA" | head -1 | cut -d= -f2-)"
  echo "finished_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "artifacts_dir=$RUN_DIR"
} >"$SUMMARY_TXT"

FINALIZED=1
echo "nightly-chaos: $OVERALL ($COMPLETED/$CHAOS_ROUNDS rounds); artifacts: $(rel_path "$RUN_DIR")"

if [[ "$OVERALL" == "PASS" ]]; then
  exit 0
fi
exit 1
