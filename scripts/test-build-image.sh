#!/usr/bin/env bash
#
# Self-test for scripts/build-image.sh.
#
# It is a shell-level gate: no registry, no network and, by default, no image
# build. A fake `docker` executable records every invocation, so the
# command-line construction, input validation and multiarch/PUSH/OCI rules are
# checked deterministically.
#
# Coverage:
#   * `bash -n` for the image scripts
#   * `docker buildx version` availability (reported, skipped when absent)
#   * MULTIARCH=1 without PUSH/OUTPUT fails with an explanation
#   * MULTIARCH=1 PUSH=1 (dry-run) uses buildx --push and no --load
#   * MULTIARCH=1 OUTPUT=oci (dry-run) writes an OCI archive, no push
#   * PLATFORMS override, invalid/duplicate/empty platform entries
#   * BUILDER / BUILDX_BUILDER, builder safety (no --use), auto-create path
#   * single-arch path still uses `docker build` and `docker push`
#   * Makefile targets: image, image-multiarch, image-multiarch-push
#
# Usage:
#   ./scripts/test-build-image.sh
#   MULTIARCH_REAL=1 ./scripts/test-build-image.sh  # also run a real OCI build
set -uo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root" || exit 1

passed=0
failed=0
skipped=0

pass() {
  passed=$((passed + 1))
  printf 'PASS: %s\n' "$1"
}

fail_test() {
  failed=$((failed + 1))
  printf 'FAIL: %s\n' "$1" >&2
  if [[ -n ${CURRENT_OUTPUT:-} ]]; then
    printf '  rc=%s\n' "${CURRENT_RC:-?}" >&2
    printf '  output (first 40 lines):\n' >&2
    printf '%s\n' "$CURRENT_OUTPUT" | sed -n '1,40p' | sed 's/^/    /' >&2
  fi
}

skip() {
  skipped=$((skipped + 1))
  printf 'SKIP: %s\n' "$1"
}

expect_rc() {
  local want=$1 desc=$2
  if [[ ${CURRENT_RC:-} -eq $want ]]; then
    pass "$desc"
  else
    fail_test "$desc (expected rc=$want, got rc=${CURRENT_RC:-?})"
  fi
}

expect_contains() {
  local needle=$1 desc=$2
  if [[ ${CURRENT_OUTPUT:-} == *"$needle"* ]]; then
    pass "$desc"
  else
    fail_test "$desc (missing substring: $needle)"
  fi
}

expect_not_contains() {
  local needle=$1 desc=$2
  if [[ ${CURRENT_OUTPUT:-} != *"$needle"* ]]; then
    pass "$desc"
  else
    fail_test "$desc (unexpected substring: $needle)"
  fi
}

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/build-image-selftest.XXXXXX") || exit 1
trap 'rm -rf "$tmp_root"' EXIT

fake_bin="$tmp_root/bin"
fake_log="$tmp_root/docker.log"
mkdir -p "$fake_bin"

# Fake docker: enough surface for the script's checks. It never talks to a
# daemon and never records credentials (there are none in these tests).
cat >"$fake_bin/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
case "${1:-}" in
  buildx)
    case "${2:-}" in
      version)
        echo "github.com/docker/buildx v0.99.0 fake"
        exit 0
        ;;
      inspect)
        # No builder name: emulate the active docker-driver builder, which
        # forces the dedicated-builder fallback. Named builders are missing
        # unless they opt in below.
        if [[ -z ${3:-} ]]; then
          echo "Name:      fake"
          echo "Driver:    docker"
          exit 0
        fi
        case "${3:-}" in
          existing-builder)
            echo "Name:      existing-builder"
            echo "Driver:    docker-container"
            exit 0
            ;;
          docker-builder)
            echo "Name:      docker-builder"
            echo "Driver:    docker"
            exit 0
            ;;
        esac
        exit 1
        ;;
      create)
        echo "fake builder created"
        exit 0
        ;;
      imagetools)
        if [[ ${3:-} == "--help" ]]; then
          echo "Usage: fake imagetools"
          exit 0
        fi
        echo "Name:      fake"
        echo "MediaType: application/vnd.oci.image.index.v1+json"
        echo "Platforms:"
        echo "  linux/amd64"
        echo "  linux/arm64"
        exit 0
        ;;
      build)
        # Emulate an OCI export so the script's post-build validation runs.
        dest=""
        for arg in "$@"; do
          case $arg in
            type=oci,dest=*,tar=true)
              dest=${arg#type=oci,dest=}
              dest=${dest%,tar=true}
              ;;
          esac
        done
        if [[ -n $dest ]]; then
          work=$(mktemp -d "${TMPDIR:-/tmp}/fake-oci.XXXXXX") || exit 1
          cat >"$work/index.json" <<'FAKE_INDEX'
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":1,"platform":{"os":"linux","architecture":"amd64"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":1,"platform":{"os":"linux","architecture":"arm64"}}]}
FAKE_INDEX
          tar -C "$work" -cf "$dest" index.json || exit 1
          rm -rf "$work"
        fi
        exit 0
        ;;
    esac
    ;;
  push)
    exit 0
    ;;
  build)
    exit 0
    ;;
esac
exit 0
FAKE_DOCKER
chmod +x "$fake_bin/docker"

CURRENT_OUTPUT=""
CURRENT_RC=0

run_build() {
  : >"$fake_log"
  CURRENT_OUTPUT=$(env PATH="$fake_bin:$PATH" FAKE_DOCKER_LOG="$fake_log" \
    IMAGE_REPOSITORY=distributed-db IMAGE_TAG=selftest "$@" \
    ./scripts/build-image.sh 2>&1)
  CURRENT_RC=$?
}

run_make() {
  CURRENT_OUTPUT=$(make -n "$@" 2>&1)
  CURRENT_RC=$?
}

log_is_empty() {
  [[ ! -s $fake_log ]]
}

printf '\n== shell syntax\n'
if bash -n scripts/build-image.sh; then
  pass "bash -n scripts/build-image.sh"
else
  fail_test "bash -n scripts/build-image.sh"
fi
if bash -n scripts/test-build-image.sh; then
  pass "bash -n scripts/test-build-image.sh"
else
  fail_test "bash -n scripts/test-build-image.sh"
fi

printf '\n== Dockerfile multi-arch guards\n'
if grep -v '^[[:space:]]*#' Dockerfile | grep -Fq 'FROM --platform=$BUILDPLATFORM'; then
  fail_test "build stage must not pin FROM to BUILDPLATFORM (empty on plain docker build)"
else
  pass "build stage does not depend on the automatic BUILDPLATFORM arg"
fi
if grep -Fq 'CGO_ENABLED=0' Dockerfile; then
  pass "Dockerfile builds with CGO_ENABLED=0"
else
  fail_test "Dockerfile does not set CGO_ENABLED=0"
fi
if grep -Fq 'GOARCH="$target_arch"' Dockerfile && grep -Fq 'GOOS="$target_os"' Dockerfile; then
  pass "Dockerfile propagates TARGETOS/TARGETARCH to go build"
else
  fail_test "Dockerfile does not propagate TARGETOS/TARGETARCH to go build"
fi

printf '\n== docker buildx\n'
if command -v docker >/dev/null 2>&1; then
  if output=$(docker buildx version 2>&1) && [[ $output == *buildx* ]]; then
    printf 'INFO: %s\n' "$output"
    pass "docker buildx version is available"
  else
    skip "docker buildx version unavailable: ${output:-no output}"
  fi
else
  skip "docker executable not installed"
fi

printf '\n== multiarch validation\n'
run_build MULTIARCH=1
expect_rc 2 "MULTIARCH=1 without PUSH/OUTPUT is rejected"
expect_contains "requires PUSH=1 or OUTPUT=oci" "error explains the push/OCI requirement"
expect_contains "cannot --load a multi-platform image" "error explains why --load is impossible"
if log_is_empty; then
  pass "no docker invocation before validation failure"
else
  fail_test "docker was invoked despite invalid input"
fi

run_build MULTIARCH=1 PUSH=1 OUTPUT=oci
expect_rc 2 "PUSH=1 + OUTPUT=oci is rejected as ambiguous"

run_build MULTIARCH=1 OUTPUT=oci PLATFORMS=amd64
expect_rc 2 "platform without os/arch is rejected"
expect_contains "invalid platform 'amd64'" "invalid platform is named in the error"

run_build MULTIARCH=1 OUTPUT=oci "PLATFORMS=linux/amd64,,linux/arm64"
expect_rc 2 "empty platform entry is rejected"
expect_contains "empty entry" "empty platform entry is named in the error"

run_build MULTIARCH=1 OUTPUT=oci PLATFORMS=linux/amd64,linux/amd64
expect_rc 2 "duplicate platform is rejected"
expect_contains "duplicate platform" "duplicate platform is named in the error"

run_build MULTIARCH=1 OUTPUT=oci OCI_DEST=relative.oci.tar
expect_rc 2 "relative OCI_DEST is rejected"

run_build MULTIARCH=2
expect_rc 2 "MULTIARCH must be 0 or 1"

printf '\n== multiarch push (dry-run)\n'
run_build MULTIARCH=1 PUSH=1 DRY_RUN=1
expect_rc 0 "multiarch push dry-run succeeds"
expect_contains "dry-run command: docker buildx build" "uses docker buildx build"
expect_contains "--platform linux/amd64,linux/arm64" "defaults to amd64+arm64"
expect_contains "--push" "passes --push"
expect_contains "--tag distributed-db:selftest" "tags the image index"
expect_not_contains "--load" "never passes --load for multiarch"
if log_is_empty; then
  pass "dry-run does not invoke docker"
else
  fail_test "dry-run invoked docker"
fi

run_build MULTIARCH=1 PUSH=1 DRY_RUN=1 PULL=1
expect_contains "--pull" "PULL=1 is passed through"

printf '\n== multiarch OCI archive (dry-run)\n'
run_build MULTIARCH=1 OUTPUT=oci DRY_RUN=1
expect_rc 0 "multiarch OCI dry-run succeeds"
expect_contains "output=oci dest=/tmp/distributed-db_selftest-multiarch.oci.tar" "default archive path is stable"
expect_contains "type=oci,dest=/tmp/distributed-db_selftest-multiarch.oci.tar,tar=true" "writes a single OCI tar"
expect_not_contains "--push" "OCI archive mode does not push"

run_build MULTIARCH=1 OUTPUT=oci OCI_DEST=/tmp/custom.oci.tar PLATFORMS=linux/amd64,linux/ppc64le DRY_RUN=1
expect_rc 0 "explicit OCI_DEST/PLATFORMS dry-run succeeds"
expect_contains "--platform linux/amd64,linux/ppc64le" "PLATFORMS override is used"
expect_contains "dest=/tmp/custom.oci.tar" "OCI_DEST override is used"

run_build MULTIARCH=1 OUTPUT=oci PLATFORMS=linux/arm64 DRY_RUN=1
expect_contains "--platform linux/arm64" "single platform still works in multiarch mode"

printf '\n== builder selection\n'
run_build MULTIARCH=1 OUTPUT=oci DRY_RUN=1 BUILDER=selftest-builder
expect_rc 0 "BUILDER dry-run succeeds"
expect_contains "--builder selftest-builder" "BUILDER is passed to buildx"
expect_not_contains "--use" "BUILDER never mutates the active builder with --use"

run_build MULTIARCH=1 OUTPUT=oci DRY_RUN=1 BUILDX_BUILDER=env-builder
expect_contains "--builder env-builder" "BUILDX_BUILDER is honoured"

# Non-dry-run builder paths use the fake docker. inspect always fails, so the
# auto-create path is exercised up to the (still missing) builder check.
run_build MULTIARCH=1 OUTPUT=oci BUILDER=missing-builder BUILDX_AUTO_CREATE=0
expect_rc 1 "missing builder with BUILDX_AUTO_CREATE=0 fails"
expect_contains "not found (BUILDX_AUTO_CREATE=0)" "failure names BUILDX_AUTO_CREATE=0"
if grep -q "buildx create" "$fake_log" 2>/dev/null; then
  fail_test "builder was created despite BUILDX_AUTO_CREATE=0"
else
  pass "builder is not created when auto-create is disabled"
fi

run_build MULTIARCH=1 OUTPUT=oci BUILDER=new-builder BUILDX_AUTO_CREATE=1
expect_rc 1 "auto-created builder that stays unavailable fails"
expect_contains "still unavailable after create" "failure explains the post-create check"
if grep -q "buildx create --name new-builder --driver docker-container" "$fake_log" 2>/dev/null; then
  pass "auto-create uses the requested name and driver"
else
  fail_test "auto-create command was not issued as expected"
fi
if grep -q -- "--use" "$fake_log" 2>/dev/null; then
  fail_test "auto-create used --use and would mutate global builder state"
else
  pass "auto-create does not use --use"
fi

# The active fake builder uses the docker driver, which cannot build
# multi-platform images. The script must fall back to a dedicated builder.
run_build MULTIARCH=1 OUTPUT=oci
expect_rc 1 "docker-driver active builder triggers a dedicated-builder fallback"
expect_contains "active buildx builder uses the docker driver" "docker-driver limitation is reported"
if grep -q "buildx create --name distributed-db-multiarch" "$fake_log" 2>/dev/null; then
  pass "fallback creates the default distributed-db-multiarch builder"
else
  fail_test "fallback did not create the default builder"
fi
if grep -q -- "--use" "$fake_log" 2>/dev/null; then
  fail_test "fallback used --use"
else
  pass "fallback does not use --use"
fi

run_build MULTIARCH=1 OUTPUT=oci BUILDX_AUTO_CREATE=0
expect_rc 1 "docker-driver active builder fails when auto-create is disabled"
expect_contains "set BUILDER=<docker-container builder>" "error tells the operator to provide a builder"

run_build MULTIARCH=1 PUSH=1 BUILDER=docker-builder
expect_rc 1 "explicit builder with the docker driver is rejected"
expect_contains "uses the docker driver" "error names the unsupported driver"

printf '\n== multiarch publish path (fake docker)\n'
run_build MULTIARCH=1 PUSH=1 BUILDER=existing-builder
expect_rc 0 "multiarch push path succeeds against the fake docker"
expect_contains "verifying registry manifest with docker buildx imagetools inspect" "push runs imagetools inspection"
expect_contains "Platforms:" "manifest platforms are printed"
expect_contains "ok mode=multiarch image=distributed-db:selftest platforms=linux/amd64,linux/arm64 pushed=1" "push reports success and platforms"
if grep -q "buildx build .*--push" "$fake_log" 2>/dev/null; then
  pass "push path uses buildx --push"
else
  fail_test "push path did not use buildx --push"
fi
if grep -q "push distributed-db:selftest" "$fake_log" 2>/dev/null; then
  fail_test "multiarch push path incorrectly falls back to docker push"
else
  pass "multiarch push path does not use docker push"
fi
if grep -q "buildx imagetools inspect distributed-db:selftest" "$fake_log" 2>/dev/null; then
  pass "pushed manifest is inspected by tag"
else
  fail_test "pushed manifest was not inspected"
fi

printf '\n== multiarch OCI validation path (fake docker)\n'
run_build MULTIARCH=1 OUTPUT=oci BUILDER=existing-builder OCI_DEST="$tmp_root/fake.oci.tar"
expect_rc 0 "OCI archive validation succeeds against the fake archive"
expect_contains "oci platforms=linux/amd64,linux/arm64" "both requested platforms are found in the index"
expect_contains "offline OCI image index" "OCI archive output is reported"

printf '\n== single-arch path (unchanged)\n'
run_build DRY_RUN=1
expect_rc 0 "single-arch dry-run succeeds"
expect_contains "mode=single-arch" "single-arch mode is reported"
expect_contains "docker build --file Dockerfile --tag distributed-db:selftest" "still uses docker build"
expect_not_contains "buildx" "single-arch path does not use buildx"

run_build DRY_RUN=1 PUSH=1
expect_contains "docker push distributed-db:selftest" "single-arch PUSH=1 still uses docker push"
expect_not_contains "buildx" "single-arch push does not use buildx"

run_build DRY_RUN=1 PLATFORM=linux/arm64
expect_contains "--platform linux/arm64" "single-arch PLATFORM is passed through"

run_build DRY_RUN=1 PLATFORMS=linux/amd64,linux/arm64
expect_rc 2 "PLATFORMS without MULTIARCH=1 is rejected"
expect_contains "PLATFORMS requires MULTIARCH=1" "error points at MULTIARCH=1"

run_build DRY_RUN=1 PUSH=2
expect_rc 2 "invalid PUSH value is rejected"
expect_contains "PUSH must be 0 or 1" "invalid PUSH is named in the error"

printf '\n== Makefile targets\n'
run_make image IMAGE_TAG=selftest
expect_rc 0 "make -n image succeeds"
expect_contains "PLATFORM=\"\"" "image target keeps PLATFORM"
expect_not_contains "MULTIARCH=1" "image target stays single-arch"

run_make image-multiarch IMAGE_TAG=selftest
expect_rc 0 "make -n image-multiarch succeeds"
expect_contains "MULTIARCH=1" "image-multiarch enables multiarch"
expect_contains "OUTPUT=oci" "image-multiarch writes an OCI archive"
expect_contains "PLATFORMS=\"linux/amd64,linux/arm64\"" "image-multiarch defaults to amd64+arm64"
expect_contains "OCI_DEST=\"/tmp/distributed-db-selftest-multiarch.oci.tar\"" "image-multiarch archive path follows the tag"
expect_not_contains "PUSH=1" "image-multiarch does not push"

run_make image-multiarch-push IMAGE_TAG=selftest
expect_rc 0 "make -n image-multiarch-push succeeds"
expect_contains "MULTIARCH=1" "image-multiarch-push enables multiarch"
expect_contains "PUSH=1" "image-multiarch-push pushes"
expect_not_contains "OUTPUT=oci" "image-multiarch-push does not write an archive"

if [[ ${MULTIARCH_REAL:-0} == "1" ]]; then
  printf '\n== real multiarch OCI build (MULTIARCH_REAL=1)\n'
  real_dest="$tmp_root/real-multiarch.oci.tar"
  real_tag="selftest-$$"
  CURRENT_OUTPUT=$(make image-multiarch IMAGE_TAG="$real_tag" OCI_DEST="$real_dest" 2>&1)
  CURRENT_RC=$?
  expect_rc 0 "real make image-multiarch build succeeds"
  if [[ -s $real_dest ]]; then
    pass "real multiarch OCI archive exists"
    if command -v python3 >/dev/null 2>&1; then
      if tar -xOf "$real_dest" index.json | python3 -c '
import json, sys
idx = json.load(sys.stdin)
plats = set()
for m in idx.get("manifests", []):
    p = m.get("platform") or {}
    if p.get("os") and p.get("architecture"):
        plats.add(p["os"] + "/" + p["architecture"])
want = {"linux/amd64", "linux/arm64"}
if not want.issubset(plats):
    raise SystemExit("index platforms %s do not include %s" % (sorted(plats), sorted(want)))
print("index platforms: " + ",".join(sorted(plats)))
'; then
        pass "real OCI index contains linux/amd64 and linux/arm64"
      else
        fail_test "real OCI index is missing a requested platform"
      fi
    else
      skip "python3 unavailable for real OCI index verification"
    fi
  else
    fail_test "real multiarch OCI archive is missing"
  fi
fi

printf '\n== summary ==\n'
printf 'passed=%d failed=%d skipped=%d\n' "$passed" "$failed" "$skipped"
if [[ $failed -ne 0 ]]; then
  exit 1
fi
exit 0
