#!/usr/bin/env bash
#
# Build (and optionally push) the multi-binary distributed-db image.
#
# The tag and the VERSION/REVISION build args are derived from the checkout,
# not from the wall clock, so rebuilding the same commit produces the same
# tag and metadata:
#
#   IMAGE_REPOSITORY  default: distributed-db
#   IMAGE_TAG         default: `git describe --tags --always --dirty`
#   VERSION           default: same as IMAGE_TAG (image label)
#   REVISION          default: full `git rev-parse HEAD` (image label)
#   PLATFORM          optional, e.g. linux/arm64 (passed to docker build)
#   PULL              default: 0; set to 1 to add --pull
#   PUSH              default: 0; set to 1 to push after a successful build
#   DOCKER            default: docker
#
# Every value may be overridden from the environment or the make command line:
#
#   IMAGE_REPOSITORY=ghcr.io/acme/distributed-db IMAGE_TAG=v2.0.0 \
#     ./scripts/build-image.sh
#
# The script does not create a registry repository and never logs in; push is
# an explicit opt-in after the operator has authenticated `docker login`.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

DOCKER=${DOCKER:-docker}
IMAGE_REPOSITORY=${IMAGE_REPOSITORY:-distributed-db}
PUSH=${PUSH:-0}
PLATFORM=${PLATFORM:-}

if ! command -v "$DOCKER" >/dev/null 2>&1; then
  echo "build-image: docker executable not found: $DOCKER" >&2
  exit 1
fi

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
  echo "build-image: invalid IMAGE_TAG '$IMAGE_TAG' (docker tag grammar)" >&2
  exit 2
fi
if [[ -z $IMAGE_REPOSITORY ]]; then
  echo "build-image: IMAGE_REPOSITORY must not be empty" >&2
  exit 2
fi
if [[ $PUSH != "0" && $PUSH != "1" ]]; then
  echo "build-image: PUSH must be 0 or 1 (got '$PUSH')" >&2
  exit 2
fi

image="${IMAGE_REPOSITORY}:${IMAGE_TAG}"

build_args=(
  --build-arg "VERSION=${VERSION}"
  --build-arg "REVISION=${REVISION}"
)

cmd=("$DOCKER" build --file Dockerfile --tag "$image")
if [[ -n $PLATFORM ]]; then
  cmd+=(--platform "$PLATFORM")
fi
if [[ ${PULL:-0} == "1" ]]; then
  cmd+=(--pull)
fi
cmd+=("${build_args[@]}" .)

# Cache mounts require BuildKit. Modern Docker defaults to it, but be explicit
# so a host with DOCKER_BUILDKIT=0 does not fail with "unknown flag --mount".
export DOCKER_BUILDKIT=1

echo "build-image: repository=${IMAGE_REPOSITORY}"
echo "build-image: tag=${IMAGE_TAG}"
echo "build-image: version=${VERSION}"
echo "build-image: revision=${REVISION}"
if [[ -n $PLATFORM ]]; then
  echo "build-image: platform=${PLATFORM}"
fi

"${cmd[@]}"

if [[ $PUSH == "1" ]]; then
  echo "build-image: pushing ${image}"
  "$DOCKER" push "$image"
fi

echo "build-image: ok image=${image}"
