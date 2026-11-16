#!/usr/bin/env bash
# Starts a local 3-node Raft cluster and the coordinator in the background.
# Logs go to ./data/logs, PIDs to ./data/pids. Ctrl-C stops everything.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin data/logs data/pids

echo "building..."
CGO_ENABLED=0 go build -trimpath -o bin/db ./cmd/db

pids=()
start() {
  local name=$1; shift
  echo "starting $name"
  "$@" >"data/logs/$name.log" 2>&1 &
  pids+=("$!")
  echo "$!" >"data/pids/$name.pid"
}

cleanup() {
  echo
  echo "stopping..."
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf data/pids
}
trap cleanup EXIT INT TERM

start node1 ./bin/db node -id=127.0.0.1:7001 -shard=shard-1 \
  -raft-addr=127.0.0.1:7001 -http-addr=:8001 \
  -peers=127.0.0.1:7002,127.0.0.1:7003 -data-dir=./data/node1 \
  -auth-user=admin -auth-pass=admin -log-level=debug
start node2 ./bin/db node -id=127.0.0.1:7002 -shard=shard-1 \
  -raft-addr=127.0.0.1:7002 -http-addr=:8002 \
  -peers=127.0.0.1:7001,127.0.0.1:7003 -data-dir=./data/node2 \
  -auth-user=admin -auth-pass=admin -log-level=debug
start node3 ./bin/db node -id=127.0.0.1:7003 -shard=shard-1 \
  -raft-addr=127.0.0.1:7003 -http-addr=:8003 \
  -peers=127.0.0.1:7001,127.0.0.1:7002 -data-dir=./data/node3 \
  -auth-user=admin -auth-pass=admin -log-level=debug
start coordinator ./bin/db coordinator -http-addr=:8080 -grpc-addr=:9090 \
  -shards='shard-1=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003' \
  -auth-user=admin -auth-pass=admin -log-level=debug

echo "waiting for the coordinator..."
for i in $(seq 1 100); do
  if curl -fsS http://127.0.0.1:8080/health >/dev/null 2>&1; then
    echo "ready: http://127.0.0.1:8080  (admin/admin)"
    echo "logs: data/logs/*.log"
    wait
    exit 0
  fi
  sleep 0.2
done

echo "cluster did not become healthy; see data/logs/*.log" >&2
exit 1
