#!/usr/bin/env bash
# Demo-profile Compose smoke test.
#
# What it proves (against the real containers, no mocks):
#   1. `docker compose config` accepts the compose file and selected profile.
#   2. The demo profile starts, meta elects a leader and the KV node answers
#      `kvctl` requests.
#   3. The range map is bootstrapped (compose bootstrap service or
#      `kvctl init-ranges`).
#   4. `CREATE TABLE`, `INSERT` and `SELECT` work through the HTTP SQL
#      endpoint, and the selected value is asserted.
#   5. A KV restart preserves the value: `kvctl get` and SQL `SELECT` succeed
#      after the restart.
#   6. Cleanup runs `docker compose down -v` unless KEEP=1.
#
# Configuration:
#   COMPOSE_FILE       compose file (default: auto-detect; demo first)
#   COMPOSE_PROFILE    profile name (default: demo)
#   PROJECT            compose project name (default: distributed-db-compose-smoke)
#   SQL_URL            base SQL URL (default: host port of the sqlsrv service;
#                      its port also seeds DB_SQL_PORT when unset)
#   TIMEOUT            seconds per wait phase (default: 180)
#   COMPOSE_UP_TIMEOUT seconds for `docker compose up` (default TIMEOUT+120)
#   DB_META_PORT       host meta port (default: 19090, avoids the 9090 dev port)
#   DB_KV_PORT         host KV port (default: 17001)
#   DB_SQL_PORT        host SQL port (default: 18080)
#   KEEP=1             leave the stack running (no down -v)
#   BUILD=1            pass --build to `docker compose up`
#   SQL_TOKEN          optional Bearer token for /v1/sql
#   SQL_INSECURE=1     pass -k to curl (self-signed TLS only)
#   META_SERVICE       override the meta service name
#   KV_RESTART_SERVICE override the KV service restarted in step 5
#   KV_NODES           override the replica addresses passed to kvctl
#   KVCTL_ADMIN_TOKEN  admin token for init-ranges (if the cluster enables it)
#   USE_BOOTSTRAP_SERVICE=1  run the compose bootstrap service instead of
#                             calling `kvctl init-ranges` directly
#
# Exit codes: 0 pass, 1 test failure, 2 environment/dependency failure.
set -Eeuo pipefail

SCRIPT_NAME=compose-smoke
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$SCRIPT_DIR/.."
# shellcheck source=compose-lib.sh
source "$SCRIPT_DIR/compose-lib.sh"

COMPOSE_PROFILE=${COMPOSE_PROFILE:-demo}
PROJECT=${PROJECT:-distributed-db-compose-smoke}
TIMEOUT=${TIMEOUT:-180}
KEEP=${KEEP:-0}
BUILD=${BUILD:-0}
SQL_URL=${SQL_URL:-}
KVCTL_BIN=${KVCTL_BIN:-/app/kvctl}
SMOKE_ID=${SMOKE_ID:-$(date +%s)-$$}
TABLE="compose_smoke_$(printf '%s' "$SMOKE_ID" | tr -c 'a-z0-9' '_')"
VALUE="compose-smoke-ok-$SMOKE_ID"

# Host ports are offset from the common 9090/7001/8080 defaults so a smoke run
# can coexist with a developer stack. The compose file interpolates these.
if [[ -n ${SQL_URL:-} && -z ${DB_SQL_PORT:-} ]]; then
  sql_url_port=$(printf '%s' "$SQL_URL" | sed -n 's|^[A-Za-z][A-Za-z0-9+.-]*://[^/:]*:\([0-9][0-9]*\).*|\1|p')
  if [[ -n $sql_url_port ]]; then
    DB_SQL_PORT=$sql_url_port
  fi
fi
export DB_META_PORT=${DB_META_PORT:-19090}
export DB_KV_PORT=${DB_KV_PORT:-17001}
export DB_SQL_PORT=${DB_SQL_PORT:-18080}
[[ $TIMEOUT =~ ^[0-9]+$ ]] || die "TIMEOUT must be an integer number of seconds (got '$TIMEOUT')" 2
COMPOSE_UP_TIMEOUT=${COMPOSE_UP_TIMEOUT:-$((TIMEOUT + 120))}
[[ $COMPOSE_UP_TIMEOUT =~ ^[0-9]+$ ]] || die "COMPOSE_UP_TIMEOUT must be an integer (got '$COMPOSE_UP_TIMEOUT')" 2

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  if (( rc == 0 )) && [[ ${SMOKE_COMPLETED:-0} != 1 ]]; then
    rc=1
  fi
  set +e
  cleanup_project
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

contains() {
  local needle=$1
  shift
  local x
  for x in "$@"; do
    if [[ $x == "$needle" ]]; then
      return 0
    fi
  done
  return 1
}

join_comma() {
  local out="" x
  for x in "$@"; do
    if [[ -z $out ]]; then
      out=$x
    else
      out="$out,$x"
    fi
  done
  printf '%s' "$out"
}

require_docker
resolve_compose_file demo
init_compose "$PROJECT" "$COMPOSE_PROFILE"

log "compose file: $COMPOSE_FILE"
log "project: $PROJECT, profile: $COMPOSE_PROFILE, timeout: ${TIMEOUT}s"

# 1. Config validation: no containers are touched before this succeeds.
if ! compose config --quiet >/dev/null 2>"$SMOKE_TMPDIR/config.err"; then
  sed 's/^/  /' "$SMOKE_TMPDIR/config.err" >&2
  die "docker compose config validation failed for $COMPOSE_FILE" 2
fi
load_compose_config
classify_services

if [[ ${#META_SERVICES[@]} -eq 0 || ${#KV_SERVICES[@]} -eq 0 || ${#SQL_SERVICES[@]} -eq 0 ]]; then
  describe_services
  die "demo profile '$COMPOSE_PROFILE' does not expose the Sprint 2 services (kv2 -mode=meta, kv2 -mode=node, sqlsrv); set COMPOSE_FILE/COMPOSE_PROFILE after the compose change lands" 2
fi

META_SERVICE=${META_SERVICE:-${META_SERVICES[0]}}
KV_TARGET=${KV_RESTART_SERVICE:-${KV_SERVICES[0]}}
SQL_SERVICE=${SQL_SERVICE:-${SQL_SERVICES[0]}}
PROBE_SERVICE=${PROBE_SERVICE:-$META_SERVICE}

contains "$META_SERVICE" "${META_SERVICES[@]}" ||
  die "META_SERVICE '$META_SERVICE' is not one of: ${META_SERVICES[*]}" 2
contains "$KV_TARGET" "${KV_SERVICES[@]}" ||
  die "KV_RESTART_SERVICE '$KV_TARGET' is not one of: ${KV_SERVICES[*]}" 2
contains "$SQL_SERVICE" "${SQL_SERVICES[@]}" ||
  die "SQL_SERVICE '$SQL_SERVICE' is not one of: ${SQL_SERVICES[*]}" 2
contains "$PROBE_SERVICE" "${META_SERVICES[@]}" "${SQL_SERVICES[@]}" ||
  die "PROBE_SERVICE '$PROBE_SERVICE' must be a meta or sqlsrv service" 2

META_PORT=$(service_port "$META_SERVICE" "-addr" 9090)
SQL_PORT=$(service_port "$SQL_SERVICE" "-http-addr" 8080)
KV_PORT=$(service_port "$KV_TARGET" "-raft-addr" 7001)
META_REPLICA_COUNT=${#META_SERVICES[@]}

# Addresses used from inside a container. If the probe is a meta container,
# localhost is allowed; otherwise use compose service DNS.
META_ADDRS=()
if [[ -n ${META_ADDR:-} ]]; then
  META_ADDRS+=("$META_ADDR")
else
  if [[ $PROBE_SERVICE == "$META_SERVICE" ]]; then
    META_ADDRS+=("127.0.0.1:$META_PORT")
  fi
  for s in "${META_SERVICES[@]}"; do
    META_ADDRS+=("${s}:$META_PORT")
  done
fi
META_ADDR=${META_ADDRS[0]}

if [[ -n ${KV_NODES:-} ]]; then
  :
else
  parts=()
  for s in "${KV_SERVICES[@]}"; do
    parts+=("$(kv_service_addr "$s" "$KV_PORT")")
  done
  KV_NODES=$(join_comma "${parts[@]}")
fi
KV_REPLICA_COUNT=$(printf '%s' "$KV_NODES" | awk -F, '{print NF}')
if [[ ! $KV_REPLICA_COUNT =~ ^[0-9]+$ ]] || (( KV_REPLICA_COUNT == 0 )); then
  die "could not derive KV replica addresses; set KV_NODES" 2
fi
KV_TARGET_ADDR=$(kv_service_addr "$KV_TARGET" "$KV_PORT")

if [[ -z ${SQL_URL:-} ]]; then
  published=$(jq -r --arg s "$SQL_SERVICE" --argjson t "$SQL_PORT" '
    [.services[$s].ports[]? | select(.target == $t) | .published]
    | map(select(. != null and (. | tostring) != ""))
    | .[0] // empty
  ' <<< "$CONFIG_JSON")
  if [[ -n $published ]]; then
    published=${published##*:}
    SQL_URL="http://127.0.0.1:$published"
  fi
fi
if [[ -n ${SQL_URL:-} ]]; then
  SQL_URL=${SQL_URL%/}
fi

log "services: meta=${META_SERVICES[*]} kv=${KV_SERVICES[*]} sql=$SQL_SERVICE"
log "replicas: $KV_NODES (leader target for restart: $KV_TARGET_ADDR)"

# 2. Start the profile.
log "starting profile '$COMPOSE_PROFILE' (host ports meta=$DB_META_PORT kv=$DB_KV_PORT sql=$DB_SQL_PORT)..."
STACK_STARTED=1
up_args=(up -d)
if [[ $BUILD == 1 ]]; then
  up_args+=(--build)
fi
up_rc=0
run_with_timeout "$COMPOSE_UP_TIMEOUT" compose "${up_args[@]}" || up_rc=$?
if (( up_rc != 0 )); then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}" "${SQL_SERVICES[@]}"
  if (( up_rc == 124 )); then
    die "docker compose up timed out after ${COMPOSE_UP_TIMEOUT}s" 1
  fi
  die "docker compose up failed (exit $up_rc)" 1
fi

log "waiting for ${#META_SERVICES[@]} meta container(s)..."
if ! wait_all_services_running "${#META_SERVICES[@]}" "${META_SERVICES[@]}"; then
  dump_diagnostics "${META_SERVICES[@]}"
  die "meta containers did not become running" 1
fi
if ! wait_meta_ready; then
  dump_diagnostics "${META_SERVICES[@]}"
  die "meta did not elect a leader (kvctl status failed)" 1
fi
log "meta leader elected"

log "waiting for ${#KV_SERVICES[@]} KV container(s)..."
if ! wait_all_services_running "${#KV_SERVICES[@]}" "${KV_SERVICES[@]}"; then
  dump_diagnostics "${KV_SERVICES[@]}"
  die "KV containers did not become running" 1
fi
for s in "${KV_SERVICES[@]}"; do
  if ! wait_kv_ready "$s"; then
    dump_diagnostics "$s"
    die "KV service '$s' did not become ready" 1
  fi
done
log "KV service(s) ready"

# 3. SQL health. Prefer the configured URL; otherwise ask compose for the
# published port of the SQL service.
if [[ -z ${SQL_URL:-} ]]; then
  host_port=$(compose port "$SQL_SERVICE" "$SQL_PORT" 2>/dev/null | head -n 1)
  [[ -n $host_port ]] ||
    die "cannot determine the published SQL port for '$SQL_SERVICE'; set SQL_URL" 2
  SQL_URL="http://127.0.0.1:${host_port##*:}"
fi
log "SQL endpoint: $SQL_URL"
if ! wait_service_running "$SQL_SERVICE"; then
  dump_diagnostics "$SQL_SERVICE"
  die "SQL service '$SQL_SERVICE' did not become running" 1
fi
if ! wait_sql_health; then
  dump_diagnostics "$SQL_SERVICE"
  die "SQL /health did not become ready at $SQL_URL" 1
fi
log "SQL /health is ready"

# 4. Range map. A fresh meta starts with one range and no replicas.
log "checking the range map..."
if ! ensure_ranges; then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}"
  die "range bootstrap failed (tried kvctl init-ranges with $KV_NODES)" 1
fi
if ! RANGES_JSON=$(wait_range_map "$KV_REPLICA_COUNT"); then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}"
  die "range map does not have $KV_REPLICA_COUNT replica(s)" 1
fi
log "range map:"
jq -r '.[] | "  range \(.id): leader=\(if ((.leader // "") == "") then "<not exposed>" else .leader end) replicas=\((.replicas // []) | join(","))"' \
  <<< "$RANGES_JSON" >&2

# 5. Meta/KV health: status already proved the meta path; prove the data path
# independently of SQL with kvctl.
log "testing meta/KV health via kvctl status + put/get..."
if ! wait_kv_roundtrip; then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}"
  die "kvctl put/get round-trip failed" 1
fi
log "KV data path verified"

# 6. SQL lifecycle through the HTTP endpoint.
log "SQL: CREATE TABLE $TABLE"
if ! run_sql "CREATE TABLE $TABLE (id INT PRIMARY KEY, v TEXT)" >/dev/null; then
  dump_diagnostics "$SQL_SERVICE"
  die "CREATE TABLE failed" 1
fi

log "SQL: INSERT INTO $TABLE"
insert_result=$(run_sql "INSERT INTO $TABLE VALUES (1, '$VALUE')") ||
  die "INSERT failed" 1
if ! jq -e '.rows_affected == 1' >/dev/null 2>&1 <<< "$insert_result"; then
  warn "INSERT result: $insert_result"
  die "INSERT did not report rows_affected=1" 1
fi

log "SQL: SELECT v FROM $TABLE WHERE id = 1"
if ! assert_sql_value "SELECT v FROM $TABLE WHERE id = 1" "$VALUE"; then
  dump_diagnostics "$SQL_SERVICE"
  die "SELECT did not return the inserted value '$VALUE'" 1
fi
log "SQL value verified: $VALUE"

# 7. Restart KV once and read the same value again. This is the persistence
# assertion: a restart must not lose the row.
log "restarting KV service '$KV_TARGET' once..."
if ! compose restart "$KV_TARGET" >/dev/null 2>&1; then
  dump_diagnostics "$KV_TARGET"
  die "docker compose restart '$KV_TARGET' failed" 1
fi
if ! wait_service_running "$KV_TARGET"; then
  dump_diagnostics "$KV_TARGET"
  die "KV service '$KV_TARGET' did not come back after restart" 1
fi
if ! wait_meta_ready; then
  dump_diagnostics "${META_SERVICES[@]}"
  die "meta status failed after the KV restart" 1
fi
if ! RANGES_JSON=$(wait_range_map "$KV_REPLICA_COUNT"); then
  dump_diagnostics "${META_SERVICES[@]}" "$KV_TARGET"
  die "range map did not keep $KV_REPLICA_COUNT replica(s) after the KV restart" 1
fi
if ! wait_kv_roundtrip; then
  dump_diagnostics "$KV_TARGET"
  die "kvctl put/get failed after the KV restart" 1
fi
if ! sql_wait_value "SELECT v FROM $TABLE WHERE id = 1" "$VALUE"; then
  dump_diagnostics "$SQL_SERVICE" "$KV_TARGET"
  die "SQL SELECT failed or lost the value after the KV restart" 1
fi
log "value survived the KV restart"

log "demo compose smoke passed"
SMOKE_COMPLETED=1
printf 'PASS: demo compose smoke (project %s, file %s, sql %s)\n' \
  "$PROJECT" "$COMPOSE_FILE" "$SQL_URL"
