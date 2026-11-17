#!/usr/bin/env bash
# Run the speed benchmarks inside a Linux Go container.
#
# This is a convenience harness for comparing macOS fsync behaviour with Linux.
# It is NOT a multi-host, NVMe or real-network benchmark: the container shares
# the host disk and CPU. Use dedicated Linux/NVMe machines for production SLOs.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

IMAGE="${GO_IMAGE:-golang:1.23-bookworm}"
PATTERN="${BENCH_PATTERN:-BenchmarkV2Txn3Node(Sync)?$|BenchmarkV2Txn3Node(Parallel|SyncParallel)$}"
TIME="${BENCH_TIME:-2s}"
COUNT="${BENCH_COUNT:-1}"

echo "image: $IMAGE"
echo "pattern: $PATTERN"
docker run --rm -v "$ROOT_DIR":/src -w /src \
  -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod -e GOFLAGS=-mod=mod \
  "$IMAGE" \
  go test -run '^$' -bench "$PATTERN" -benchtime="$TIME" -count="$COUNT" ./test
