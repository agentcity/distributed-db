#!/usr/bin/env bash
#
# Automated logical backup/restore drill.
#
# Starts no external server and contacts no cluster: the Go test builds an
# in-process meta server and KV node, seeds data, takes a real logical backup,
# restores it into a fresh in-process cluster, and verifies every expected key.
#
# Usage:
#   ./scripts/restore-drill.sh
#   ./scripts/restore-drill.sh --report-dir ./drill-report
#   ./scripts/restore-drill.sh --pitr          # optional archive-level PITR path
#   DRILL_REPORT_DIR=/tmp/drill ./scripts/restore-drill.sh
#
# Environment:
#   DRILL_REPORT_DIR  directory for report.json and test.log (default: mktemp -d)
#   DRILL_PITR        1 enables RESTORE_DRILL_PITR for the optional PITR path
#   DRILL_TIMEOUT     go test -timeout (default: 90s)
#   GO                go binary (default: go)
#
# Exit status is non-zero if the test fails or if the JSON report is missing,
# malformed, or shows a missing/mismatched key.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root" || exit 1

usage() {
  sed -n '2,/^[^#]/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
}

report_dir=${DRILL_REPORT_DIR:-}
pitr=${DRILL_PITR:-0}
test_timeout=${DRILL_TIMEOUT:-90s}
go_bin=${GO:-go}

while (($# > 0)); do
  case "$1" in
    --report-dir)
      if (($# < 2)); then
        echo "restore-drill: --report-dir requires a value" >&2
        exit 2
      fi
      report_dir=$2
      shift 2
      ;;
    --pitr)
      pitr=1
      shift
      ;;
    --timeout)
      if (($# < 2)); then
        echo "restore-drill: --timeout requires a value" >&2
        exit 2
      fi
      test_timeout=$2
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "restore-drill: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if ! command -v "$go_bin" >/dev/null 2>&1; then
  echo "FAIL: Go binary not found: $go_bin" >&2
  exit 1
fi

if [[ -z "$report_dir" ]]; then
  report_dir=$(mktemp -d "${TMPDIR:-/tmp}/restore-drill.XXXXXX")
fi
mkdir -p "$report_dir"
report_path="$report_dir/report.json"
log_path="$report_dir/test.log"
rm -f "$report_path" "$log_path"

export RESTORE_DRILL_REPORT="$report_path"
if [[ "$pitr" == "1" ]]; then
  export RESTORE_DRILL_PITR=1
else
  unset RESTORE_DRILL_PITR || true
fi

printf '== logical backup/restore drill\n'
printf 'report:   %s\n' "$report_path"
printf 'test log: %s\n' "$log_path"
printf 'command:  %s test -race -count=1 -timeout %s ./test -run ^TestRestoreDrill$\n' \
  "$go_bin" "$test_timeout"

set +e
"$go_bin" test -race -count=1 -v -timeout "$test_timeout" ./test -run '^TestRestoreDrill$' 2>&1 | tee "$log_path"
test_status=${PIPESTATUS[0]}
set -e

if [[ ! -s "$report_path" ]]; then
  echo "FAIL: test did not write a JSON report at $report_path" >&2
  if [[ -s "$log_path" ]]; then
    echo "--- test log (tail) ---" >&2
    tail -n 40 "$log_path" >&2
  fi
  exit 1
fi

validate_report() {
  if command -v jq >/dev/null 2>&1; then
    jq -e '
      .status == "pass" and
      .verification.status == "pass" and
      .verification.no_missing_committed_keys == true and
      .verification.missing_keys == 0 and
      .verification.value_mismatches == 0 and
      .verification.unexpected_keys == 0 and
      .verification.deleted_key_absent == true and
      .verification.reapply_ok == true
    ' "$report_path" >/dev/null
    return
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$report_path" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    report = json.load(fh)
verification = report.get("verification") or {}
checks = [
    report.get("status") == "pass",
    verification.get("status") == "pass",
    verification.get("no_missing_committed_keys") is True,
    verification.get("missing_keys") == 0,
    verification.get("value_mismatches") == 0,
    verification.get("unexpected_keys") == 0,
    verification.get("deleted_key_absent") is True,
    verification.get("reapply_ok") is True,
]
if not all(checks):
    raise SystemExit(1)
PY
    return
  fi
  # Last-resort fallback when neither jq nor python3 is installed. The test
  # itself already fails on a mismatch; this guards the published report.
  grep -q '"missing_keys": 0' "$report_path" &&
    grep -q '"value_mismatches": 0' "$report_path" &&
    grep -q '"unexpected_keys": 0' "$report_path" &&
    grep -q '"no_missing_committed_keys": true' "$report_path" &&
    grep -q '"deleted_key_absent": true' "$report_path" &&
    grep -q '"reapply_ok": true' "$report_path" &&
    sed -n '1,20p' "$report_path" | grep -q '"status": "pass"'
}

summary() {
  if command -v jq >/dev/null 2>&1; then
    jq -r '"snapshot_ts=\(.backup.snapshot_ts) rpo_tso_lag=\(.rpo.tso_lag) restore_ms=\(.restore.duration_ms) total_ms=\(.duration_ms) pitr=\(.pitr.status)"' "$report_path"
    return
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$report_path" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    r = json.load(fh)
pitr = (r.get("pitr") or {}).get("status", "?")
print(
    "snapshot_ts=%s rpo_tso_lag=%s restore_ms=%s total_ms=%s pitr=%s"
    % (
        (r.get("backup") or {}).get("snapshot_ts"),
        (r.get("rpo") or {}).get("tso_lag"),
        (r.get("restore") or {}).get("duration_ms"),
        r.get("duration_ms"),
        pitr,
    )
)
PY
    return
  fi
  grep -E '"(snapshot_ts|tso_lag|duration_ms)"' "$report_path" || true
}

if validate_report; then
  report_ok=1
else
  report_ok=0
fi

if ((test_status != 0)); then
  echo "FAIL: go test exited with status $test_status" >&2
  echo "--- test log (tail) ---" >&2
  tail -n 40 "$log_path" >&2
  exit 1
fi

if ((report_ok == 0)); then
  echo "FAIL: report verification failed (missing/mismatched key or non-pass status)" >&2
  summary >&2 || true
  exit 1
fi

echo "PASS: logical backup/restore drill"
summary
echo "report: $report_path"
echo "log:    $log_path"
