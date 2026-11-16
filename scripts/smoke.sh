#!/usr/bin/env bash
# End-to-end smoke test against a running coordinator.
#   COORDINATOR_URL=http://127.0.0.1:8080 AUTH=admin:admin ./scripts/smoke.sh
set -euo pipefail

BASE="${COORDINATOR_URL:-http://127.0.0.1:8080}"
AUTH="${AUTH:-admin:admin}"
KEY="${SMOKE_KEY:-smoke-$(date +%s)}"

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "waiting for $BASE/health ..."
for _ in $(seq 1 100); do
  if curl -fsS "$BASE/health" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS "$BASE/health" >/dev/null || fail "coordinator is not healthy"

echo "PUT $KEY"
code=$(curl -sS -o /tmp/db-smoke-put.out -w '%{http_code}' -u "$AUTH" \
  -X PUT --data-binary 'hello raft' "$BASE/v1/kv/$KEY")
case "$code" in 200|201|204) ;; *) fail "PUT returned $code: $(cat /tmp/db-smoke-put.out)";; esac

echo "GET $KEY"
got=$(curl -fsS -u "$AUTH" "$BASE/v1/kv/$KEY")
[[ "$got" == "hello raft" ]] || fail "GET returned '$got'"

echo "DELETE $KEY"
code=$(curl -sS -o /tmp/db-smoke-del.out -w '%{http_code}' -u "$AUTH" \
  -X DELETE "$BASE/v1/kv/$KEY")
case "$code" in 200|204) ;; *) fail "DELETE returned $code: $(cat /tmp/db-smoke-del.out)";; esac

echo "GET after DELETE (expect 404)"
code=$(curl -sS -o /dev/null -w '%{http_code}' -u "$AUTH" "$BASE/v1/kv/$KEY")
[[ "$code" == "404" ]] || fail "GET after DELETE returned $code"

rm -f /tmp/db-smoke-put.out /tmp/db-smoke-del.out
echo "PASS"
