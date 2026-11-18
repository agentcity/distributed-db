#!/usr/bin/env bash
#
# Local dry-run for the release automation.
#
# This script validates the release inputs without contacting a registry, a
# Kubernetes cluster, or GitHub:
#   * Bash syntax of the image build/release scripts and POSIX syntax of the
#     chart startup scripts;
#   * the multi-arch build command via scripts/build-image.sh DRY_RUN=1 with a
#     fake docker executable (the fake must never be invoked);
#   * Dockerfile and build-script assumptions used by the release workflow;
#   * YAML/JSON parsing of the release workflow and chart values;
#   * chart invariants with helm lint/template/package when Helm is installed;
#   * values.schema.json rejects invalid replica counts, an empty TLS Secret
#     reference, and an unpinned "latest" image tag;
#   * the release workflow's tag gate, GHCR login, multi-arch build, digest,
#     artifact and least-privilege permissions via a small YAML contract check;
#   * actionlint when it is installed.
#
# Nothing here pushes an image, publishes a chart, creates a GitHub release, or
# applies anything to a cluster. A passing run is not evidence that a release
# was executed.
#
# Usage:
#   ./scripts/verify-release.sh
#   HELM=/path/to/helm ./scripts/verify-release.sh
#   REQUIRE_HELM=1 ./scripts/verify-release.sh   # missing Helm is fatal
#
# Exit status: 0 when every executed check passed, 1 otherwise. Missing optional
# tools are reported as skips; missing Helm is a skip unless REQUIRE_HELM=1.
#
# SC2016: the Dockerfile check intentionally matches the literal ${GO_VERSION}
# text, and the short bash -c snippets intentionally defer expansion to bash.
# shellcheck disable=SC2016
set -uo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root" || exit 1

HELM=${HELM:-helm}
CHART=${CHART:-deploy/helm/distributed-db}
RELEASE=${RELEASE:-distributed-db}
NAMESPACE=${NAMESPACE:-distributed-db}
WORKFLOW=${WORKFLOW:-.github/workflows/release.yml}
REQUIRE_HELM=${REQUIRE_HELM:-0}

failures=0
skips=0
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

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

run_expect_schema_failure() {
  local desc=$1
  shift
  local out="$tmpdir/expect-schema.out"
  printf '\n== %s\n' "$desc"
  if "$@" >"$out" 2>&1; then
    printf 'FAIL: %s (command succeeded unexpectedly)\n' "$desc" >&2
    failures=$((failures + 1))
    return
  fi
  if grep -q "values don't meet the specifications of the schema" "$out"; then
    printf 'PASS: %s\n' "$desc"
  else
    printf 'FAIL: %s (failed, but not because of values.schema.json)\n' "$desc" >&2
    sed -n '1,20p' "$out" >&2
    failures=$((failures + 1))
  fi
}

yaml_parser_available() {
  if command -v ruby >/dev/null 2>&1; then
    return 0
  fi
  if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

yaml_parse() {
  local file=$1
  if [[ ! -f $file ]]; then
    echo "missing YAML file: $file" >&2
    return 1
  fi
  if command -v ruby >/dev/null 2>&1; then
    ruby -ryaml -e '
      begin
        docs = YAML.load_stream(File.read(ARGV[0]))
      rescue Psych::Exception => e
        warn "yaml parse error in #{ARGV[0]}: #{e.message}"
        exit 1
      end
      docs = docs.compact
      if docs.empty?
        warn "no YAML documents in #{ARGV[0]}"
        exit 1
      end
      puts "  yaml documents=#{docs.size}"
    ' "$file"
    return
  fi
  if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "$file" <<'PY'
import sys
import yaml
with open(sys.argv[1], encoding="utf-8") as fh:
    docs = [d for d in yaml.safe_load_all(fh) if d is not None]
if not docs:
    raise SystemExit(f"no YAML documents in {sys.argv[1]}")
print(f"  yaml documents={len(docs)}")
PY
    return
  fi
  echo "no YAML parser found (need ruby, or python3 with PyYAML)" >&2
  return 1
}

json_parse() {
  local file=$1
  if [[ ! -f $file ]]; then
    echo "missing JSON file: $file" >&2
    return 1
  fi
  if command -v ruby >/dev/null 2>&1; then
    ruby -rjson -e 'JSON.parse(File.read(ARGV[0])); puts "  json ok"' "$file"
  elif command -v jq >/dev/null 2>&1; then
    jq empty "$file" && echo "  json ok"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -m json.tool "$file" >/dev/null && echo "  json ok"
  else
    echo "no JSON parser found" >&2
    return 1
  fi
}

check_dockerfile() {
  local f=Dockerfile
  local needle
  for needle in \
    '# syntax=docker/dockerfile' \
    'FROM golang:${GO_VERSION}-bookworm AS build' \
    'FROM alpine:3.20 AS runtime' \
    'ARG TARGETOS' \
    'ARG TARGETARCH' \
    'CGO_ENABLED=0' \
    'USER 10001:10001' \
    'ENTRYPOINT ["/app/db"]'; do
    if ! grep -Fq -- "$needle" "$f"; then
      echo "Dockerfile is missing: $needle" >&2
      return 1
    fi
  done
  for binary in kv2 sqlsrv pgsrv kvctl kvbackup edgehash; do
    if ! grep -Fq -- "/app/${binary}" "$f"; then
      echo "Dockerfile does not document /app/${binary}" >&2
      return 1
    fi
  done
}

check_build_script_contract() {
  local f=scripts/build-image.sh
  local needle
  for needle in 'MULTIARCH' 'PLATFORMS' 'DRY_RUN' 'docker buildx build' '--push'; do
    if ! grep -Fq -- "$needle" "$f"; then
      echo "build-image.sh is missing: $needle" >&2
      return 1
    fi
  done
}

check_build_dry_run() {
  local fake_docker="$tmpdir/fake-docker"
  local docker_log="$tmpdir/docker-calls"
  local out="$tmpdir/build-dry-run.out"

  cat > "$fake_docker" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$docker_log"
exit 1
EOF
  chmod +x "$fake_docker"

  # DRY_RUN=1 must print the multi-arch push command and must not invoke the
  # docker executable at all. The fake records any call and exits non-zero.
  if ! DOCKER="$fake_docker" \
    DRY_RUN=1 \
    MULTIARCH=1 \
    PUSH=1 \
    PLATFORMS=linux/amd64,linux/arm64 \
    IMAGE_REPOSITORY=ghcr.io/example/distributed-db \
    IMAGE_TAG=v9.9.9 \
    VERSION=v9.9.9 \
    REVISION=0123456789abcdef0123456789abcdef01234567 \
    ./scripts/build-image.sh >"$out" 2>&1; then
    echo "build-image.sh dry-run failed:" >&2
    sed -n '1,80p' "$out" >&2
    return 1
  fi

  if ! grep -Fq -- '--platform linux/amd64,linux/arm64' "$out"; then
    echo "dry-run command does not contain the requested platforms" >&2
    cat "$out" >&2
    return 1
  fi
  if ! grep -Fq -- '--push' "$out"; then
    echo "dry-run command does not show --push" >&2
    cat "$out" >&2
    return 1
  fi
  if ! grep -Fq 'ghcr.io/example/distributed-db:v9.9.9' "$out"; then
    echo "dry-run command does not contain the requested image reference" >&2
    cat "$out" >&2
    return 1
  fi
  if [[ -s $docker_log ]]; then
    echo "dry-run invoked the fake docker executable (a real run could have pushed):" >&2
    cat "$docker_log" >&2
    return 1
  fi
  printf '  %s\n' "$(grep -m1 'dry-run command' "$out")"
  echo "  fake docker was not invoked; no image was built or pushed"
}

check_chart_files() {
  local f
  for f in \
    "$CHART/Chart.yaml" \
    "$CHART/values.yaml" \
    "$CHART/values.schema.json" \
    "$CHART/templates/NOTES.txt" \
    "$CHART/templates/_helpers.tpl" \
    "$CHART/files/meta-start.sh" \
    "$CHART/files/kv-start.sh" \
    "$CHART/files/sqlsrv-start.sh" \
    "$CHART/files/pgsrv-start.sh" \
    "$CHART/files/range-bootstrap.sh"; do
    if [[ ! -f $f ]]; then
      echo "missing chart file: $f" >&2
      return 1
    fi
  done
}

check_values_no_literal_secret() {
  local values="$CHART/values.yaml"
  # Only Secret references and key names belong here. A top-level password or
  # token value would be a credential committed to Git.
  if grep -En '^[[:space:]]*(password|passwd|token|secret|adminTokenValue):[[:space:]]*[^[:space:]#]' "$values"; then
    echo "values.yaml appears to contain a literal secret value" >&2
    return 1
  fi
  if ! grep -Fq 'existingSecret:' "$values"; then
    echo "values.yaml has no Secret references" >&2
    return 1
  fi
}

check_schema_contract() {
  local schema="$CHART/values.schema.json"
  local key
  json_parse "$schema" || return 1
  for key in '"image"' '"replicaCount"' '"existingSecret"' '"bootstrap"' '"mtls"'; do
    if ! grep -Fq -- "$key" "$schema"; then
      echo "values.schema.json does not cover $key" >&2
      return 1
    fi
  done
  if grep -Eq '"default"[[:space:]]*:' "$schema"; then
    echo "values.schema.json must not define defaults (especially secret values)" >&2
    return 1
  fi
}

workflow_contract_check() {
  if [[ ! -f $WORKFLOW ]]; then
    echo "missing release workflow: $WORKFLOW" >&2
    return 1
  fi
  if ! command -v ruby >/dev/null 2>&1; then
    echo "ruby is needed for the workflow contract check" >&2
    return 1
  fi
  ruby -ryaml - "$WORKFLOW" <<'RUBY'
require "yaml"

path = ARGV.fetch(0)
raw = File.read(path)

begin
  doc = YAML.safe_load(raw, aliases: true)
rescue Psych::Exception => e
  abort "workflow YAML parse failed: #{e.message}"
end
abort "workflow must be a YAML mapping" unless doc.is_a?(Hash)

on_key = if doc.key?("on")
           "on"
         elsif doc.key?(true)
           true
         end
abort "workflow is missing the on trigger" unless on_key
triggers = doc[on_key]
abort "workflow on must be a mapping" unless triggers.is_a?(Hash)
push = triggers["push"]
abort "workflow must trigger on push.tags" unless push.is_a?(Hash)
abort "workflow must trigger on the v* tag pattern" unless Array(push["tags"]).include?("v*")

unless doc["permissions"] == { "contents" => "read" }
  abort "top-level permissions must be exactly contents: read"
end

jobs = doc["jobs"]
abort "workflow jobs must be a mapping" unless jobs.is_a?(Hash)
%w[validate image release].each do |name|
  abort "workflow is missing job '#{name}'" unless jobs[name].is_a?(Hash)
end

def step_with_run(job, needle)
  Array(job["steps"]).find { |step| step["run"].to_s.include?(needle) }
end

def step_with_uses(job, needle)
  Array(job["steps"]).find { |step| step["uses"].to_s.include?(needle) }
end

validate = jobs["validate"]
tag_step = step_with_run(validate, "GITHUB_REF_TYPE")
abort "validate job is missing the tag guard" unless tag_step
tag_run = tag_step["run"].to_s
abort "tag guard must verify a local refs/tags entry" unless tag_run.include?("refs/tags/")
abort "tag guard must fail on a missing tag" unless tag_run.include?("missing from the checkout")
abort "validate job must run scripts/verify-release.sh" unless step_with_run(validate, "scripts/verify-release.sh")
abort "validate job must lint the chart" unless step_with_run(validate, "helm lint")
abort "validate job must package the chart" unless step_with_run(validate, "helm package")
abort "validate job must upload the chart artifact" unless step_with_uses(validate, "actions/upload-artifact@")

image = jobs["image"]
image_permissions = image["permissions"]
unless image_permissions.is_a?(Hash) &&
       image_permissions["contents"] == "read" &&
       image_permissions["packages"] == "write"
  abort "image job permissions must be contents: read and packages: write"
end
abort "image job must set up QEMU" unless step_with_uses(image, "docker/setup-qemu-action@")
abort "image job must set up Buildx" unless step_with_uses(image, "docker/setup-buildx-action@")

login = step_with_uses(image, "docker/login-action@")
abort "image job must log in to GHCR" unless login
login_with = login["with"] || {}
abort "GHCR login registry must be ghcr.io" unless login_with["registry"] == "ghcr.io"
unless login_with["password"].to_s.include?("secrets.GITHUB_TOKEN")
  abort "GHCR login must use GITHUB_TOKEN, not a long-lived registry password"
end

build = step_with_uses(image, "docker/build-push-action@")
abort "image job must use docker/build-push-action" unless build
build_with = build["with"] || {}
abort "build must push" unless build_with["push"] == true
platforms = build_with["platforms"].to_s
%w[linux/amd64 linux/arm64].each do |platform|
  abort "build platforms must include #{platform}" unless platforms.include?(platform)
end
abort "build must enable an SBOM" unless build_with["sbom"] == true
abort "build must request provenance" unless build_with.key?("provenance")

record = step_with_run(image, "docker buildx imagetools inspect")
abort "image job must verify the pushed image with imagetools" unless record
record_run = record["run"].to_s
abort "image job must write image-digest.txt" unless record_run.include?("image-digest.txt")
abort "image job must check both platforms" unless record_run.include?("linux/amd64") && record_run.include?("linux/arm64")
abort "image job must upload image metadata" unless step_with_uses(image, "actions/upload-artifact@")

release = jobs["release"]
unless release["permissions"] == { "contents" => "write" }
  abort "release job permissions must be exactly contents: write"
end
abort "release job must download the chart artifact" unless step_with_uses(release, "actions/download-artifact@")
publish = step_with_run(release, "gh release create")
abort "release job must create a GitHub release" unless publish
publish_run = publish["run"].to_s
abort "release must attach the chart .tgz" unless publish_run.include?("dist/*.tgz")
abort "release must attach image-digest.txt" unless publish_run.include?("image-digest.txt")
unless publish_run.include?("--verify-tag")
  abort "gh release create must use --verify-tag"
end

raw.each_line do |line|
  next unless line =~ /^\s*password:\s*(.*)$/

  value = Regexp.last_match(1).strip
  unless value.start_with?("${{")
    abort "workflow contains a literal password value"
  end
end
if raw.match?(/\bdocker\s+login\s+-p\b/)
  abort "workflow must not pass a docker password on the command line"
end

puts "  workflow contract ok: tag gate, GHCR login, amd64+arm64, SBOM/provenance,"
puts "  digest verification, chart package, release assets, least-privilege permissions"
RUBY
}

# --- static files and syntax -------------------------------------------------
run_check "bash -n release/build scripts" \
  bash -n scripts/build-image.sh scripts/verify-release.sh
run_check "sh -n chart startup/bootstrap scripts" \
  sh -n "$CHART"/files/meta-start.sh "$CHART"/files/kv-start.sh \
    "$CHART"/files/sqlsrv-start.sh "$CHART"/files/pgsrv-start.sh \
    "$CHART"/files/range-bootstrap.sh
run_check "Dockerfile release contract" check_dockerfile
run_check "build-image.sh release contract" check_build_script_contract
run_check "chart files present" check_chart_files
run_check "values.yaml contains references, not literal secrets" check_values_no_literal_secret
run_check "values.schema.json contract" check_schema_contract

if command -v shellcheck >/dev/null 2>&1; then
  run_check "shellcheck release/build scripts" \
    shellcheck scripts/build-image.sh scripts/verify-release.sh
  run_check "shellcheck chart scripts (POSIX sh)" \
    shellcheck -s sh "$CHART"/files/*.sh
else
  skip_check "shellcheck is not installed (bash -n / sh -n passed above)"
fi

# --- workflow contract -------------------------------------------------------
if command -v ruby >/dev/null 2>&1; then
  run_check "release workflow contract and YAML structure" workflow_contract_check
else
  skip_check "ruby is not installed; workflow contract and YAML structure check skipped"
fi

if command -v actionlint >/dev/null 2>&1; then
  run_check "actionlint release workflow" actionlint "$WORKFLOW"
else
  skip_check "actionlint is not installed (install it to lint GitHub Actions expressions)"
fi

# --- parse YAML/JSON ---------------------------------------------------------
if yaml_parser_available; then
  for yamlfile in "$WORKFLOW" "$CHART/Chart.yaml" "$CHART/values.yaml"; do
    run_check "YAML parse: $yamlfile" yaml_parse "$yamlfile"
  done
else
  skip_check "no YAML parser (ruby, or python3 with PyYAML); static YAML parse skipped"
fi
run_check "JSON parse: $CHART/values.schema.json" json_parse "$CHART/values.schema.json"

# --- image build dry-run (no Docker daemon, no push) -------------------------
run_check "multi-arch build dry-run (fake docker, no push)" check_build_dry_run

# --- Helm -------------------------------------------------------------------
if command -v "$HELM" >/dev/null 2>&1; then
  run_check "helm lint --strict (default values)" \
    "$HELM" lint --strict "$CHART"
  run_check "helm lint --strict (bootstrap enabled)" \
    "$HELM" lint --strict "$CHART" --set bootstrap.enabled=true
  run_check "helm lint --strict (no TLS / no auth)" \
    "$HELM" lint --strict "$CHART" \
      --set tls.enabled=false \
      --set tls.mtls=false \
      --set sqlsrv.auth.enabled=false \
      --set pgsrv.auth.enabled=false \
      --set meta.adminToken.enabled=false

  render_and_parse() {
    local outfile=$1
    shift
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" "$@" > "$outfile" || return 1
    yaml_parse "$outfile"
  }
  run_check "render+parse default values" \
    render_and_parse "$tmpdir/default.yaml"
  run_check "render+parse bootstrap enabled" \
    render_and_parse "$tmpdir/bootstrap.yaml" --set bootstrap.enabled=true
  run_check "render+parse release image/tag" \
    render_and_parse "$tmpdir/release.yaml" \
      --set image.repository=ghcr.io/example/distributed-db \
      --set image.tag=v9.9.9
  run_check "render+parse no-TLS/no-auth" \
    render_and_parse "$tmpdir/no-tls.yaml" \
      --set tls.enabled=false \
      --set tls.mtls=false \
      --set sqlsrv.auth.enabled=false \
      --set pgsrv.auth.enabled=false \
      --set meta.adminToken.enabled=false

  package_dir="$tmpdir/package"
  mkdir -p "$package_dir"
  run_check "helm package" \
    "$HELM" package "$CHART" --version 9.9.9 --app-version 9.9.9 --destination "$package_dir"
  package_file=$(find "$package_dir" -maxdepth 1 -name '*.tgz' -print -quit)
  if [[ -n $package_file ]]; then
    run_check "packaged chart contains Chart.yaml" \
      bash -c 'tar -tzf "$1" | grep -qx "distributed-db/Chart.yaml"' _ "$package_file"
    run_check "packaged chart renders" \
      bash -c '"$1" template "$2" "$3" --namespace "$4" >/dev/null' _ \
        "$HELM" "$RELEASE" "$package_file" "$NAMESPACE"
  else
    printf '\nFAIL: helm package produced no .tgz\n' >&2
    failures=$((failures + 1))
  fi

  # These must fail specifically because the values schema rejects them.
  run_expect_schema_failure "schema rejects meta.replicaCount=4" \
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set meta.replicaCount=4
  run_expect_schema_failure "schema rejects kv.replicaCount=9" \
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set kv.replicaCount=9
  run_expect_schema_failure "schema rejects empty TLS Secret reference" \
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set tls.existingSecret=
  run_expect_schema_failure "schema rejects an unpinned latest image tag" \
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set image.tag=latest
  run_expect_schema_failure "schema rejects a literal admin token value" \
    "$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set auth.adminToken.token=not-a-secret-reference
else
  if [[ $REQUIRE_HELM == "1" ]]; then
    run_check "Helm is required but not installed" false
  else
    skip_check "Helm is not installed (set HELM=/path/to/helm or REQUIRE_HELM=1 to make it fatal)"
  fi
fi

printf '\n== summary\n'
printf 'failures=%d skips=%d\n' "$failures" "$skips"
if (( failures > 0 )); then
  printf 'release dry-run FAILED\n' >&2
  exit 1
fi
printf 'release dry-run passed\n'
printf 'NOTE: this was a local dry-run only. No image was built or pushed, no chart was published,\n'
printf '      no GitHub release was created, and no cluster was contacted.\n'
