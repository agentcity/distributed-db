#!/usr/bin/env bash
#
# Build (and optionally push/publish) the multi-binary distributed-db image.
#
# The tag and the VERSION/REVISION build args are derived from the checkout,
# not from the wall clock, so rebuilding the same commit produces the same
# tag and metadata:
#
#   IMAGE_REPOSITORY  default: distributed-db
#   IMAGE_TAG         default: `git describe --tags --always --dirty`
#   VERSION           default: same as IMAGE_TAG (image label)
#   REVISION          default: full `git rev-parse HEAD` (image label)
#   PLATFORM          optional single-arch platform, e.g. linux/arm64
#   PULL              default: 0; set to 1 to add --pull
#   PUSH              default: 0; set to 1 to publish the result
#   DOCKER            default: docker
#
# Multi-arch mode (MULTIARCH=1) uses `docker buildx build`:
#
#   PLATFORMS         comma-separated list, default: linux/amd64,linux/arm64
#   PUSH=1            build and push a multi-platform image index (--push).
#   OUTPUT=oci        build a multi-platform OCI archive instead of pushing.
#   OCI_DEST          archive path, default: /tmp/<repo>-<tag>-multiarch.oci.tar
#   BUILDER           optional buildx builder name (alias: BUILDX_BUILDER)
#   BUILDX_AUTO_CREATE default: 1; create the named builder if it is missing
#   BUILDX_DRIVER     default: docker-container (only used on auto-create)
#   BUILDX_DEFAULT_BUILDER
#                     default: distributed-db-multiarch; builder created when
#                     the active builder uses the docker driver, which cannot
#                     build multi-platform images
#   DRY_RUN=1         print the commands without invoking Docker
#
# buildx cannot load a multi-platform result into the local image store
# (`--load` only accepts one platform). Therefore MULTIARCH=1 requires either
# PUSH=1 (publish the image index) or OUTPUT=oci (write an offline OCI
# archive). There is no silent fallback: without one of them the script exits
# with an explanatory error. Building an OCI archive needs no registry login,
# which makes multi-arch validation possible on an isolated host.
#
# The script does not create a registry repository and never logs in; push is
# an explicit opt-in after the operator has authenticated `docker login`.
# Registry credentials are never read or printed by this script.
#
# Examples:
#
#   # Single-arch local build
#   ./scripts/build-image.sh
#
#   # Multi-arch image index in a local OCI archive (no registry)
#   MULTIARCH=1 OUTPUT=oci OCI_DEST=/tmp/distributed-db.oci.tar \
#     ./scripts/build-image.sh
#
#   # Multi-arch build and push (requires `docker login` first)
#   MULTIARCH=1 PUSH=1 IMAGE_REPOSITORY=ghcr.io/acme/distributed-db \
#     IMAGE_TAG=v2.0.0 ./scripts/build-image.sh
#
#   # Build through a dedicated buildx builder
#   MULTIARCH=1 PUSH=1 BUILDER=distributed-db-builder ./scripts/build-image.sh
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

DOCKER=${DOCKER:-docker}
IMAGE_REPOSITORY=${IMAGE_REPOSITORY:-distributed-db}
PUSH=${PUSH:-0}
PULL=${PULL:-0}
PLATFORM=${PLATFORM:-}
MULTIARCH=${MULTIARCH:-0}
PLATFORMS=${PLATFORMS:-}
OUTPUT=${OUTPUT:-}
OCI_DEST=${OCI_DEST:-}
BUILDER=${BUILDER:-${BUILDX_BUILDER:-}}
BUILDX_AUTO_CREATE=${BUILDX_AUTO_CREATE:-1}
BUILDX_DRIVER=${BUILDX_DRIVER:-docker-container}
BUILDX_DEFAULT_BUILDER=${BUILDX_DEFAULT_BUILDER:-distributed-db-multiarch}
DRY_RUN=${DRY_RUN:-0}

fail() {
  echo "build-image: $*" >&2
  exit 1
}

usage_error() {
  echo "build-image: $*" >&2
  exit 2
}

if ! command -v "$DOCKER" >/dev/null 2>&1; then
  fail "docker executable not found: $DOCKER"
fi

# Reject typos instead of silently treating them as "false".
for bool_var in PUSH PULL MULTIARCH DRY_RUN BUILDX_AUTO_CREATE; do
  bool_value=${!bool_var}
  if [[ $bool_value != "0" && $bool_value != "1" ]]; then
    usage_error "$bool_var must be 0 or 1 (got '$bool_value')"
  fi
done

# git may be unavailable in a release tarball. Fall back to stable literals
# instead of embedding a timestamp or an empty value.
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
  REVISION=${REVISION:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
else
  VERSION=${VERSION:-dev}
  REVISION=${REVISION:-unknown}
fi
IMAGE_TAG=${IMAGE_TAG:-$VERSION}

# Docker tag grammar: [A-Za-z0-9_][A-Za-z0-9_.-]{0,127}. Do not silently
# rewrite an invalid tag: a release pipeline must fail loudly.
if [[ ! $IMAGE_TAG =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
  usage_error "invalid IMAGE_TAG '$IMAGE_TAG' (docker tag grammar)"
fi
if [[ -z $IMAGE_REPOSITORY ]]; then
  usage_error "IMAGE_REPOSITORY must not be empty"
fi

image="${IMAGE_REPOSITORY}:${IMAGE_TAG}"

build_args=(
  --build-arg "VERSION=${VERSION}"
  --build-arg "REVISION=${REVISION}"
)

# Cache mounts require BuildKit. Modern Docker defaults to it, but be explicit
# so a host with DOCKER_BUILDKIT=0 does not fail with "unknown flag --mount".
export DOCKER_BUILDKIT=1

echo "build-image: repository=${IMAGE_REPOSITORY}"
echo "build-image: tag=${IMAGE_TAG}"
echo "build-image: version=${VERSION}"
echo "build-image: revision=${REVISION}"

# ---------------------------------------------------------------------------
# Single-platform path (unchanged behaviour)
# ---------------------------------------------------------------------------
if [[ $MULTIARCH == "0" ]]; then
  if [[ -n $PLATFORMS ]]; then
    usage_error "PLATFORMS requires MULTIARCH=1; set MULTIARCH=1 or use PLATFORM for a single platform"
  fi
  if [[ -n $OUTPUT || -n $OCI_DEST ]]; then
    usage_error "OUTPUT/OCI_DEST require MULTIARCH=1"
  fi
  if [[ -n $BUILDER ]]; then
    usage_error "BUILDER requires MULTIARCH=1 (single-platform builds use the default builder)"
  fi

  cmd=("$DOCKER" build --file Dockerfile --tag "$image")
  if [[ -n $PLATFORM ]]; then
    cmd+=(--platform "$PLATFORM")
    echo "build-image: platform=${PLATFORM}"
  fi
  if [[ $PULL == "1" ]]; then
    cmd+=(--pull)
  fi
  cmd+=("${build_args[@]}" .)

  if [[ $DRY_RUN == "1" ]]; then
    echo "build-image: mode=single-arch (dry-run, Docker is not invoked)"
    echo "build-image: dry-run command: ${cmd[*]}"
    if [[ $PUSH == "1" ]]; then
      echo "build-image: dry-run command: ${DOCKER} push ${image}"
    fi
    exit 0
  fi

  "${cmd[@]}"

  if [[ $PUSH == "1" ]]; then
    echo "build-image: pushing ${image}"
    "$DOCKER" push "$image"
  fi

  echo "build-image: ok mode=single-arch image=${image}${PLATFORM:+ platform=${PLATFORM}}"
  exit 0
fi

# ---------------------------------------------------------------------------
# Multi-platform path (buildx)
# ---------------------------------------------------------------------------
if [[ -n $PLATFORM ]]; then
  usage_error "PLATFORM is single-arch only; use PLATFORMS with MULTIARCH=1"
fi
if [[ $PUSH == "1" && -n $OUTPUT ]]; then
  usage_error "PUSH=1 and OUTPUT=${OUTPUT} are mutually exclusive; choose push or a local archive"
fi

PLATFORMS=${PLATFORMS:-linux/amd64,linux/arm64}

# Validate the comma-separated platform list before invoking buildx. Keep the
# original order and reject duplicates: a duplicated platform would silently
# produce a confusing index.
platform_list=""
platform_count=0
IFS=',' read -r -a requested_platforms <<<"$PLATFORMS"
for requested in "${requested_platforms[@]}"; do
  if [[ -z $requested ]]; then
    usage_error "PLATFORMS contains an empty entry: '$PLATFORMS'"
  fi
  if [[ ! $requested =~ ^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)?$ ]]; then
    usage_error "invalid platform '$requested' in PLATFORMS (expected os/arch[/variant], e.g. linux/amd64)"
  fi
  case ",$platform_list," in
    *",$requested,"*) usage_error "duplicate platform '$requested' in PLATFORMS" ;;
  esac
  if [[ -n $platform_list ]]; then
    platform_list="${platform_list},${requested}"
  else
    platform_list=$requested
  fi
  platform_count=$((platform_count + 1))
done
if [[ $platform_count -eq 0 ]]; then
  usage_error "PLATFORMS must contain at least one platform"
fi
PLATFORMS=$platform_list

if [[ $PUSH == "0" && -z $OUTPUT ]]; then
  usage_error "MULTIARCH=1 requires PUSH=1 or OUTPUT=oci: buildx cannot --load a multi-platform image (use PUSH=1 to publish, or OUTPUT=oci OCI_DEST=/tmp/<name>.tar for an offline archive)"
fi

case $OUTPUT in
  "" | "oci") ;;
  *) usage_error "OUTPUT must be 'oci' (got '$OUTPUT'); multi-platform local --load is not supported" ;;
esac

if [[ $OUTPUT == "oci" ]]; then
  if [[ -z $OCI_DEST ]]; then
    # Keep the default stable per repository/tag and outside the build context.
    sanitized_image=$(printf '%s' "$image" | tr '/:@ ' '____')
    OCI_DEST="/tmp/${sanitized_image}-multiarch.oci.tar"
  fi
  case $OCI_DEST in
    /*) ;;
    *) usage_error "OCI_DEST must be an absolute path (got '$OCI_DEST')" ;;
  esac
fi

echo "build-image: mode=multiarch"
echo "build-image: platforms=${PLATFORMS}"
echo "build-image: push=${PUSH}"
if [[ $OUTPUT == "oci" ]]; then
  echo "build-image: output=oci dest=${OCI_DEST}"
else
  echo "build-image: output=registry (multi-platform image index)"
fi

builder=${BUILDER:-}

# The legacy `docker` driver cannot build multi-platform images (it is tied to
# the daemon's image store). When no builder was requested and the active
# builder uses that driver, fall back to a dedicated docker-container builder.
# This is only a fallback: an explicitly named builder is never replaced.
if [[ $DRY_RUN == "0" && -z $builder ]]; then
  active_driver=$("$DOCKER" buildx inspect 2>/dev/null | awk '$1 == "Driver:" { print $2; exit }' || true)
  if [[ $active_driver == "docker" ]]; then
    if [[ $BUILDX_AUTO_CREATE != "1" ]]; then
      fail "the active buildx builder uses the docker driver, which does not support multi-platform builds; set BUILDER=<docker-container builder> or BUILDX_AUTO_CREATE=1 (the script would then create '${BUILDX_DEFAULT_BUILDER}')"
    fi
    builder=$BUILDX_DEFAULT_BUILDER
    echo "build-image: active buildx builder uses the docker driver (no multi-platform support); using builder '${builder}'"
  fi
fi

builder_args=()
if [[ -n $builder ]]; then
  echo "build-image: builder=${builder}"
  builder_args=(--builder "$builder")
  if [[ $DRY_RUN == "0" ]]; then
    if ! "$DOCKER" buildx inspect "$builder" >/dev/null 2>&1; then
      if [[ $BUILDX_AUTO_CREATE != "1" ]]; then
        fail "buildx builder '$builder' not found (BUILDX_AUTO_CREATE=0); create it with: docker buildx create --name $builder --driver $BUILDX_DRIVER"
      fi
      # Auto-create only an explicitly named (or default fallback) builder and
      # never with --use: the build passes --builder, so the operator's active
      # builder and global docker state are left untouched.
      echo "build-image: builder '$builder' not found; creating it (driver=${BUILDX_DRIVER}, using --builder, not --use)"
      if ! "$DOCKER" buildx create --name "$builder" --driver "$BUILDX_DRIVER" >/dev/null; then
        fail "could not create buildx builder '$builder'; create it manually: docker buildx create --name $builder --driver $BUILDX_DRIVER"
      fi
      if ! "$DOCKER" buildx inspect "$builder" >/dev/null 2>&1; then
        fail "buildx builder '$builder' is still unavailable after create; inspect it with: docker buildx inspect --bootstrap $builder"
      fi
    fi
    builder_driver=$("$DOCKER" buildx inspect "$builder" 2>/dev/null | awk '$1 == "Driver:" { print $2; exit }' || true)
    if [[ $builder_driver == "docker" ]]; then
      fail "buildx builder '$builder' uses the docker driver, which cannot build multi-platform images; create a container driver builder: docker buildx create --name ${builder}-container --driver docker-container"
    fi
  fi
fi

cmd=("$DOCKER" buildx build --file Dockerfile --tag "$image" --platform "$PLATFORMS")
if [[ ${#builder_args[@]} -gt 0 ]]; then
  cmd+=("${builder_args[@]}")
fi
if [[ $PULL == "1" ]]; then
  cmd+=(--pull)
fi
if [[ $PUSH == "1" ]]; then
  cmd+=(--push)
else
  # tar=true keeps the output a single archive file (OCI image layout inside)
  # so it can be copied between hosts and inspected with tar/python.
  cmd+=(--output "type=oci,dest=${OCI_DEST},tar=true")
fi
cmd+=("${build_args[@]}" .)

if [[ $DRY_RUN == "1" ]]; then
  echo "build-image: dry-run command: ${cmd[*]}"
  exit 0
fi

buildx_version=$("$DOCKER" buildx version 2>/dev/null) || fail "MULTIARCH=1 requires docker buildx; '$DOCKER buildx version' failed"
echo "build-image: buildx=${buildx_version}"

"${cmd[@]}"

# ---------------------------------------------------------------------------
# Post-build validation
# ---------------------------------------------------------------------------
# OCI archives are validated locally: the archive must contain an OCI image
# index with one manifest per requested platform. Registry pushes are
# validated with `docker buildx imagetools inspect` when that command exists.
oci_platforms() {
  local archive=$1
  if [[ ! -s $archive ]]; then
    fail "OCI archive missing or empty: ${archive}"
  fi
  if ! tar -xOf "$archive" index.json >/dev/null 2>&1; then
    fail "OCI archive has no index.json: ${archive}"
  fi
  if command -v python3 >/dev/null 2>&1; then
    # A multi-platform `type=oci` export writes an index whose single entry
    # points at the image index (one level down, in blobs/sha256/<digest>).
    # Walk nested image indexes and collect every platform descriptor.
    python3 - "$archive" <<'PY' || fail "could not parse OCI index: ${archive}"
import json
import sys
import tarfile

archive = sys.argv[1]
found = set()
try:
    tf = tarfile.open(archive, mode="r:*")
except Exception as exc:
    print("build-image: cannot open OCI archive: %s" % exc, file=sys.stderr)
    sys.exit(1)


def load(name):
    try:
        member = tf.getmember(name)
    except KeyError:
        return None
    handle = tf.extractfile(member)
    if handle is None:
        return None
    try:
        return json.load(handle)
    except Exception:
        return None


def walk(index, depth=0):
    if depth > 4:
        return
    for manifest in index.get("manifests", []):
        platform = manifest.get("platform") or {}
        os_name = platform.get("os")
        arch = platform.get("architecture")
        if os_name and arch and os_name != "unknown" and arch != "unknown":
            parts = [os_name, arch]
            if platform.get("variant"):
                parts.append(platform["variant"])
            found.add("/".join(parts))
        media_type = manifest.get("mediaType", "")
        digest = manifest.get("digest", "")
        if media_type.endswith("image.index.v1+json") and digest.startswith("sha256:"):
            nested = load("blobs/sha256/" + digest.split(":", 1)[1])
            if nested is not None:
                walk(nested, depth + 1)


root = load("index.json")
if root is None:
    print("build-image: invalid OCI index.json", file=sys.stderr)
    sys.exit(1)
walk(root)
print(",".join(sorted(found)))
PY
  elif command -v jq >/dev/null 2>&1; then
    # jq can only see the top-level index without shelling out per blob. That
    # is enough for single-level indexes; a nested multi-platform index is
    # reported as unknown so the caller does not pretend it was verified.
    top_level=$(tar -xOf "$archive" index.json | jq -r '
      [.manifests[]?
       | select(.platform.os != null and .platform.os != "unknown")
       | select(.platform.architecture != null and .platform.architecture != "unknown")
       | (.platform.os + "/" + .platform.architecture
          + (if .platform.variant then "/" + .platform.variant else "" end))]
      | unique | join(",")
    ') || fail "could not parse OCI index: ${archive}"
    if [[ -n $top_level ]]; then
      printf '%s' "$top_level"
    else
      echo "build-image: warning: jq cannot traverse nested OCI indexes; skipping per-platform verification (install python3 for full validation)" >&2
      printf '%s' "unknown"
    fi
  else
    # No JSON parser available: the archive and index.json were found, but the
    # per-platform descriptors cannot be checked. Say so instead of pretending.
    echo "build-image: warning: neither python3 nor jq is installed; skipping per-platform OCI index verification" >&2
    printf '%s' "unknown"
  fi
}

if [[ $PUSH == "1" ]]; then
  echo "build-image: manifest=${image} (multi-platform OCI image index)"
  if "$DOCKER" buildx imagetools --help >/dev/null 2>&1; then
    echo "build-image: verifying registry manifest with docker buildx imagetools inspect"
    if ! "$DOCKER" buildx imagetools inspect "$image"; then
      fail "push finished but manifest verification failed for ${image}"
    fi
  else
    echo "build-image: warning: docker buildx imagetools is unavailable; the pushed manifest was not inspected" >&2
  fi
  echo "build-image: ok mode=multiarch image=${image} platforms=${PLATFORMS} pushed=1"
else
  built_platforms=$(oci_platforms "$OCI_DEST")
  echo "build-image: oci platforms=${built_platforms}"
  if [[ -z $built_platforms ]]; then
    fail "OCI index contains no platform descriptors: ${OCI_DEST}"
  fi
  # Compare os/arch/variant, accepting a variant-qualified built platform for a
  # request that omits the variant (e.g. linux/arm64 vs linux/arm64/v8).
  found_platforms=()
  if [[ $built_platforms != "unknown" ]]; then
    IFS=',' read -r -a found_platforms <<<"$built_platforms"
  fi
  for requested in "${requested_platforms[@]}"; do
    if [[ $built_platforms == "unknown" ]]; then
      break
    fi
    requested_os=${requested%%/*}
    requested_rest=${requested#*/}
    requested_arch=${requested_rest%%/*}
    requested_variant=""
    if [[ $requested_rest == */* ]]; then
      requested_variant=${requested_rest#*/}
    fi
    matched=0
    for found in ${found_platforms[@]+"${found_platforms[@]}"}; do
      found_os=${found%%/*}
      found_rest=${found#*/}
      found_arch=${found_rest%%/*}
      found_variant=""
      if [[ $found_rest == */* ]]; then
        found_variant=${found_rest#*/}
      fi
      if [[ $found_os == "$requested_os" && $found_arch == "$requested_arch" ]]; then
        if [[ -z $requested_variant || $found_variant == "$requested_variant" ]]; then
          matched=1
          break
        fi
      fi
    done
    if [[ $matched -ne 1 ]]; then
      fail "requested platform '${requested}' is missing from ${OCI_DEST} (index platforms: ${built_platforms})"
    fi
  done
  echo "build-image: manifest=${image} (offline OCI image index at ${OCI_DEST})"
  echo "build-image: ok mode=multiarch image=${image} platforms=${PLATFORMS} oci=${OCI_DEST}"
fi
