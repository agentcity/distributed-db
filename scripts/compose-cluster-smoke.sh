#!/usr/bin/env bash
# Three-node Compose cluster smoke test.
#
# What it proves (against the real containers, no mocks):
#   1. `docker compose config` accepts the cluster compose file/profile.
#   2. Exactly three meta and three KV containers start; meta elects a leader
#      and every KV listener becomes healthy. The range map then proves that
#      all three KV replicas are published.
#   3. The range map is bootstrapped (compose bootstrap service or
#      `kvctl init-ranges`), and `kvctl ranges` shows three replicas for every
#      range. The range descriptor does not currently expose a leader, so the
#      test asserts the replica set and proves availability through SQL.
#   4. `CREATE TABLE`, `INSERT` and `SELECT` work through the HTTP SQL
#      endpoint, and the selected value is asserted.
#   5. With STOP_KV=1, one KV is stopped and a new write/read still succeeds
#      with 2/3 replicas; the range map must still show three replicas. The
#      range descriptor does not expose a leader, so availability is proven by
#      the SQL write/read rather than by a leader label.
#   6. Cleanup runs `docker compose down -v` unless KEEP=1.
#
# Configuration:
#   COMPOSE_FILE       compose file (default: cluster file, else the root file)
#   COMPOSE_PROFILE    profile name (default: cluster)
#   PROJECT            compose project (default: distributed-db-cluster-smoke)
#   SQL_URL            base SQL URL (default: published sqlsrv port; its port
#                      also seeds DB_SQL_PORT when unset)
#   TIMEOUT            seconds per wait phase (default: 300)
#   COMPOSE_UP_TIMEOUT seconds for `docker compose up` (default TIMEOUT+120)
#   DB_META_PORT       host meta-1 port (default: 19091)
#   DB_KV_PORT         host kv-1 port (default: 17002)
#   DB_SQL_PORT        host SQL port (default: 18081)
#   KEEP=1             leave the stack running (no down -v)
#   BUILD=1            pass --build to `docker compose up`
#   META_REPLICAS      expected meta containers (default: 3)
#   KV_REPLICAS        expected KV containers/replicas (default: 3)
#   STOP_KV=0          skip the optional 2/3 fault-tolerance step
#   STOP_KV_SERVICE    KV service to stop (default: the last kv-* service)
#   RESTART_STOPPED=0  do not restart the stopped KV at the end
#   SQL_TOKEN          optional Bearer token for /v1/sql
#   SQL_INSECURE=1     pass -k to curl (self-signed TLS only)
#   KV_NODES           override the replica addresses passed to kvctl
#   KVCTL_ADMIN_TOKEN  admin token for init-ranges (if the cluster enables it)
#   USE_BOOTSTRAP_SERVICE=1  run the compose bootstrap service instead of
#                             calling `kvctl init-ranges` directly
#
# Exit codes: 0 pass, 1 test failure, 2 environment/dependency failure.
set -Eeuo pipefail

SCRIPT_NAME=compose-cluster-smoke
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$SCRIPT_DIR/.."
# shellcheck source=compose-lib.sh
source "$SCRIPT_DIR/compose-lib.sh"

COMPOSE_PROFILE=${COMPOSE_PROFILE:-cluster}
PROJECT=${PROJECT:-distributed-db-cluster-smoke}
TIMEOUT=${TIMEOUT:-300}
KEEP=${KEEP:-0}
BUILD=${BUILD:-0}
SQL_URL=${SQL_URL:-}
META_REPLICAS=${META_REPLICAS:-3}
KV_REPLICAS=${KV_REPLICAS:-3}
STOP_KV=${STOP_KV:-1}
RESTART_STOPPED=${RESTART_STOPPED:-1}
KVCTL_BIN=${KVCTL_BIN:-/app/kvctl}
SMOKE_ID=${SMOKE_ID:-$(date +%s)-$$}
TABLE="cluster_smoke_$(printf '%s' "$SMOKE_ID" | tr -c 'a-z0-9' '_')"
VALUE="cluster-smoke-ok-$SMOKE_ID"

# Host ports are offset from the common 9090/7001/8080 defaults so a smoke run
# can coexist with a developer stack. Only meta-1 and kv-1 publish host ports
# in the cluster compose file; the other replicas talk over the compose network.
if [[ -n ${SQL_URL:-} && -z ${DB_SQL_PORT:-} ]]; then
  sql_url_port=$(printf '%s' "$SQL_URL" | sed -n 's|^[A-Za-z][A-Za-z0-9+.-]*://[^/:]*:\([0-9][0-9]*\).*|\1|p')
  if [[ -n $sql_url_port ]]; then
    DB_SQL_PORT=$sql_url_port
  fi
fi
export DB_META_PORT=${DB_META_PORT:-19091}
export DB_KV_PORT=${DB_KV_PORT:-17002}
export DB_SQL_PORT=${DB_SQL_PORT:-18081}
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
[[ $META_REPLICAS =~ ^[0-9]+$ ]] || die "META_REPLICAS must be an integer (got '$META_REPLICAS')" 2
[[ $KV_REPLICAS =~ ^[0-9]+$ ]] || die "KV_REPLICAS must be an integer (got '$KV_REPLICAS')" 2
(( META_REPLICAS > 0 && KV_REPLICAS > 0 )) ||
  die "META_REPLICAS and KV_REPLICAS must be > 0" 2
resolve_compose_file cluster
init_compose "$PROJECT" "$COMPOSE_PROFILE"

log "compose file: $COMPOSE_FILE"
log "project: $PROJECT, profile: $COMPOSE_PROFILE, timeout: ${TIMEOUT}s"

# 1. Config validation.
if ! compose config --quiet >/dev/null 2>"$SMOKE_TMPDIR/config.err"; then
  sed 's/^/  /' "$SMOKE_TMPDIR/config.err" >&2
  die "docker compose config validation failed for $COMPOSE_FILE" 2
fi
load_compose_config
classify_services

if [[ ${#META_SERVICES[@]} -eq 0 || ${#KV_SERVICES[@]} -eq 0 || ${#SQL_SERVICES[@]} -eq 0 ]]; then
  describe_services
  die "cluster profile '$COMPOSE_PROFILE' does not expose the Sprint 2 services (kv2 -mode=meta, kv2 -mode=node, sqlsrv); set COMPOSE_FILE/COMPOSE_PROFILE after the compose change lands" 2
fi
if (( ${#META_SERVICES[@]} != META_REPLICAS )); then
  describe_services
  die "cluster profile has ${#META_SERVICES[@]} meta service(s), expected META_REPLICAS=$META_REPLICAS" 2
fi
if (( ${#KV_SERVICES[@]} != KV_REPLICAS )); then
  describe_services
  die "cluster profile has ${#KV_SERVICES[@]} KV service(s), expected KV_REPLICAS=$KV_REPLICAS" 2
fi

META_SERVICE=${META_SERVICE:-${META_SERVICES[0]}}
SQL_SERVICE=${SQL_SERVICE:-${SQL_SERVICES[0]}}
PROBE_SERVICE=${PROBE_SERVICE:-$META_SERVICE}

contains "$META_SERVICE" "${META_SERVICES[@]}" ||
  die "META_SERVICE '$META_SERVICE' is not one of: ${META_SERVICES[*]}" 2
contains "$SQL_SERVICE" "${SQL_SERVICES[@]}" ||
  die "SQL_SERVICE '$SQL_SERVICE' is not one of: ${SQL_SERVICES[*]}" 2
contains "$PROBE_SERVICE" "${META_SERVICES[@]}" "${SQL_SERVICES[@]}" ||
  die "PROBE_SERVICE '$PROBE_SERVICE' must be a meta or sqlsrv service" 2

META_PORT=$(service_port "$META_SERVICE" "-addr" 9090)
SQL_PORT=$(service_port "$SQL_SERVICE" "-http-addr" 8080)
KV_PORT=$(service_port "${KV_SERVICES[0]}" "-raft-addr" 7001)
META_REPLICA_COUNT=$META_REPLICAS

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
if [[ ! $KV_REPLICA_COUNT =~ ^[0-9]+$ ]] || (( KV_REPLICA_COUNT != KV_REPLICAS )); then
  die "KV_NODES has $KV_REPLICA_COUNT replica(s), expected KV_REPLICAS=$KV_REPLICAS ($KV_NODES)" 2
fi

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
log "replicas: $KV_NODES"
if [[ ${#BOOTSTRAP_SERVICES[@]} -gt 0 ]]; then
  log "bootstrap service present: ${BOOTSTRAP_SERVICES[*]} (USE_BOOTSTRAP_SERVICE=1 to run it)"
fi

# 2. Start the cluster profile.
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

log "waiting for $META_REPLICAS meta container(s)..."
if ! wait_all_services_running "$META_REPLICAS" "${META_SERVICES[@]}"; then
  dump_diagnostics "${META_SERVICES[@]}"
  die "meta containers did not all become running" 1
fi
if ! wait_meta_ready; then
  dump_diagnostics "${META_SERVICES[@]}"
  die "meta quorum did not elect a leader (kvctl status failed)" 1
fi
log "meta leader elected"

log "waiting for $KV_REPLICAS KV container(s)..."
if ! wait_all_services_running "$KV_REPLICAS" "${KV_SERVICES[@]}"; then
  dump_diagnostics "${KV_SERVICES[@]}"
  die "KV containers did not all become running" 1
fi
for s in "${KV_SERVICES[@]}"; do
  if ! wait_kv_ready "$s"; then
    dump_diagnostics "$s"
    die "KV service '$s' did not become ready" 1
  fi
done
log "all $KV_REPLICAS KV services ready"

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

# 3. Bootstrap and verify the range map.
log "checking the range map..."
if ! ensure_ranges; then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}"
  die "range bootstrap failed (tried kvctl init-ranges with $KV_NODES)" 1
fi
if ! RANGES_JSON=$(wait_range_map "$KV_REPLICAS"); then
  dump_diagnostics "${META_SERVICES[@]}" "${KV_SERVICES[@]}"
  die "range map does not have $KV_REPLICAS replicas for every range" 1
fi

log "kvctl ranges:"
if ! kvctl_meta ranges 2>&1 | sed 's/^/  /' >&2; then
  warn "could not print the kvctl ranges table"
fi

# 4. SQL lifecycle through the HTTP endpoint.
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

# 5. Optional fault-tolerance check: stop one KV and write with a 2/3 quorum.
# The meta range descriptor never populates Leader (no UpdateLeader caller), so
# do not assert a leader label: prove availability with a real SQL write/read.
if [[ $STOP_KV == 1 ]]; then
  if (( KV_REPLICAS < 3 )); then
    warn "STOP_KV=1 requires at least 3 replicas; skipping the 2/3 test"
  else
    STOP_SERVICE=${STOP_KV_SERVICE:-${KV_SERVICES[$((${#KV_SERVICES[@]} - 1))]}}
    contains "$STOP_SERVICE" "${KV_SERVICES[@]}" ||
      die "STOP_KV_SERVICE '$STOP_SERVICE' is not one of: ${KV_SERVICES[*]}" 2
    STOP_ADDR=$(kv_service_addr "$STOP_SERVICE" "$KV_PORT")

    log "stopping KV '$STOP_SERVICE' ($STOP_ADDR); range descriptors do not expose a leader"
    if ! compose stop "$STOP_SERVICE" >/dev/null 2>&1; then
      dump_diagnostics "$STOP_SERVICE"
      die "docker compose stop '$STOP_SERVICE' failed" 1
    fi

    remaining=()
    for s in "${KV_SERVICES[@]}"; do
      if [[ $s != "$STOP_SERVICE" ]]; then
        remaining+=("$s")
      fi
    done
    if ! wait_all_services_running "$((KV_REPLICAS - 1))" "${remaining[@]}"; then
      dump_diagnostics "${remaining[@]}"
      die "only $((KV_REPLICAS - 1)) KV services should remain, but they did not all stay running" 1
    fi

    if ! RANGES_AFTER=$(wait_range_map "$KV_REPLICAS"); then
      dump_diagnostics "${remaining[@]}" "${META_SERVICES[@]}"
      die "range map no longer shows $KV_REPLICAS replicas after stopping '$STOP_ADDR'" 1
    fi
    log "range map still shows $KV_REPLICAS replicas with $((KV_REPLICAS - 1))/$KV_REPLICAS processes up"

    keep_id=2
    keep_value="cluster-smoke-after-kv-stop-$SMOKE_ID"
    log "SQL: INSERT/SELECT with 2/3 replicas"
    if ! sql_write_then_wait_value \
      "INSERT INTO $TABLE VALUES ($keep_id, '$keep_value')" \
      "SELECT v FROM $TABLE WHERE id = $keep_id" \
      "$keep_value"; then
      dump_diagnostics "${remaining[@]}" "$SQL_SERVICE"
      die "write/read failed with 2/3 replicas after stopping '$STOP_SERVICE'" 1
    fi

    if ! range_map_ok "$RANGES_AFTER" "$KV_REPLICAS"; then
      warn "range map after stop: $RANGES_AFTER"
      die "range map no longer has $KV_REPLICAS replicas after stopping '$STOP_SERVICE'" 1
    fi
    log "2/3 write/read verified after stopping '$STOP_SERVICE'"

    if [[ $RESTART_STOPPED == 1 ]]; then
      log "restarting stopped KV '$STOP_SERVICE'..."
      if compose start "$STOP_SERVICE" >/dev/null 2>&1; then
        if ! wait_all_services_running "$KV_REPLICAS" "${KV_SERVICES[@]}"; then
          warn "stopped KV '$STOP_SERVICE' did not return; range map was still valid"
        elif ! RANGES_AFTER=$(wait_range_map "$KV_REPLICAS"); then
          warn "range map was not stable after restarting '$STOP_SERVICE'"
        else
          log "KV '$STOP_SERVICE' restarted; $KV_REPLICAS replicas visible again"
        fi
      else
        warn "docker compose start '$STOP_SERVICE' failed; leaving it stopped"
      fi
    fi
  fi
else
  log "STOP_KV=0: skipping the optional 2/3 fault-tolerance check"
fi

log "cluster compose smoke passed"
SMOKE_COMPLETED=1
printf 'PASS: cluster compose smoke (project %s, file %s, sql %s, replicas %s)\n' \
  "$PROJECT" "$COMPOSE_FILE" "$SQL_URL" "$KV_REPLICAS"
