#!/usr/bin/env bash
#
# Local release gate for the multi-binary image and the Helm chart.
#
# It checks:
#   * POSIX/Bash syntax of the build and chart bootstrap scripts;
#   * shellcheck when it is installed (otherwise the syntax check is the gate);
#   * Helm lint and render (default, bootstrap-enabled, and no-TLS/no-token);
#   * YAML parsing of every rendered document;
#   * optionally the real docker image build (WITH_IMAGE=1).
#
# Usage:
#   ./scripts/validate-release.sh
#   WITH_IMAGE=1 ./scripts/validate-release.sh
#   REQUIRE_HELM=1 ./scripts/validate-release.sh   # missing Helm is fatal
#
# Nothing here contacts a Kubernetes cluster. A chart render is not a
# deployment and does not prove that the database is production-ready.
set -uo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root" || exit 1

HELM=${HELM:-helm}
CHART=${CHART:-deploy/helm/distributed-db}
RELEASE=${RELEASE:-distributed-db}
NAMESPACE=${NAMESPACE:-distributed-db}
WITH_IMAGE=${WITH_IMAGE:-0}
REQUIRE_HELM=${REQUIRE_HELM:-0}

failures=0
skips=0

run_check() {
  local desc=$1
  shift
  printf '\n== %s\n' "$desc"
  if "$@"; then
    printf 'PASS: %s\n' "$desc"
  else
    printf 'FAIL: %s\n' "$desc" >&2
    failures=$((failures + 1))
  fi
}

skip_check() {
  printf '\nSKIP: %s\n' "$1"
  skips=$((skips + 1))
}

yaml_parse() {
  local file=$1
  if command -v ruby >/dev/null 2>&1; then
    ruby -ryaml -e '
      docs = YAML.load_stream(File.read(ARGV[0])).compact
      abort "no YAML documents in #{ARGV[0]}" if docs.empty?
      puts "  yaml documents=#{docs.size}"
    ' "$file"
  elif command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "$file" <<'PY'
import sys
import yaml
with open(sys.argv[1], encoding="utf-8") as fh:
    docs = [d for d in yaml.safe_load_all(fh) if d is not None]
if not docs:
    raise SystemExit(f"no YAML documents in {sys.argv[1]}")
print(f"  yaml documents={len(docs)}")
PY
  else
    echo "no YAML parser found (need ruby, or python3 with PyYAML)" >&2
    return 1
  fi
}

helm_render_parse() {
  local outfile=$1
  shift
  "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" "$@" > "$outfile" || return 1
  yaml_parse "$outfile"
}

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# --- shell syntax -----------------------------------------------------------
run_check "bash -n build/validation scripts" \
  bash -n scripts/build-image.sh scripts/validate-release.sh
run_check "sh -n chart startup/bootstrap scripts" \
  sh -n "$CHART"/files/meta-start.sh "$CHART"/files/kv-start.sh \
    "$CHART"/files/sqlsrv-start.sh "$CHART"/files/pgsrv-start.sh \
    "$CHART"/files/range-bootstrap.sh

if command -v shellcheck >/dev/null 2>&1; then
  run_check "shellcheck build/validation scripts" \
    shellcheck scripts/build-image.sh scripts/validate-release.sh
  run_check "shellcheck chart scripts (POSIX sh)" \
    shellcheck -s sh "$CHART"/files/*.sh
else
  skip_check "shellcheck is not installed (bash -n / sh -n passed above)"
fi

# --- Helm -------------------------------------------------------------------
if command -v "$HELM" >/dev/null 2>&1; then
  run_check "helm lint --strict (default values)" \
    "$HELM" lint --strict "$CHART"
  run_check "helm lint --strict (bootstrap enabled)" \
    "$HELM" lint --strict "$CHART" --set bootstrap.enabled=true

  run_check "render+parse default values" \
    helm_render_parse "$tmpdir/default.yaml"
  run_check "render+parse bootstrap enabled" \
    helm_render_parse "$tmpdir/bootstrap.yaml" --set bootstrap.enabled=true
  run_check "render+parse bootstrap without TLS/token" \
    helm_render_parse "$tmpdir/bootstrap-plaintext.yaml" \
      --set bootstrap.enabled=true \
      --set tls.enabled=false \
      --set tls.mtls=false \
      --set sqlsrv.auth.enabled=false \
      --set pgsrv.auth.enabled=false \
      --set meta.adminToken.enabled=false
  run_check "render+parse 5 KV replicas" \
    helm_render_parse "$tmpdir/kv5.yaml" --set kv.replicaCount=5
else
  if [[ $REQUIRE_HELM == "1" ]]; then
    run_check "Helm is required but not installed" false
  else
    skip_check "Helm is not installed (install Helm 3.14+ or set HELM=; REQUIRE_HELM=1 makes this fatal)"
  fi
fi

# --- image ------------------------------------------------------------------
if [[ $WITH_IMAGE == "1" ]]; then
  run_check "docker image build" ./scripts/build-image.sh
else
  skip_check "docker image build (set WITH_IMAGE=1 to build; not run by default)"
fi

printf '\n== summary\n'
printf 'failures=%d skips=%d\n' "$failures" "$skips"
if (( failures > 0 )); then
  exit 1
fi
printf 'release checks passed\n'
