#!/usr/bin/env bash
# Shared helpers for the Compose smoke tests.
#
# Sourced by scripts/compose-smoke.sh and scripts/compose-cluster-smoke.sh.
# The scripts are intentionally read-only with respect to the compose files:
# they discover services, ports and replicas from `docker compose config` so a
# renamed service or a new compose filename does not silently invalidate the
# test.
#
# Requirements on the host: docker (with the compose v2 plugin), curl, jq.
# Requirements in the image: /app/kvctl (the multi-binary image builds it).
#
# shellcheck shell=bash

if [[ -n ${_COMPOSE_SMOKE_LIB_LOADED:-} ]]; then
  return 0
fi
_COMPOSE_SMOKE_LIB_LOADED=1

set -Eeuo pipefail

COMPOSE_LIB_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$COMPOSE_LIB_DIR/.." && pwd)
SCRIPT_NAME=${SCRIPT_NAME:-compose-smoke}

SMOKE_TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/ddb-compose-smoke.XXXXXX") || {
  echo "ERROR: cannot create a temporary directory" >&2
  exit 2
}
STACK_STARTED=0

log()  { printf '[%s] %s\n' "$SCRIPT_NAME" "$*" >&2; }
warn() { printf '[%s] WARN: %s\n' "$SCRIPT_NAME" "$*" >&2; }
die()  {
  local msg=$1 code=${2:-1}
  printf '[%s] ERROR: %s\n' "$SCRIPT_NAME" "$msg" >&2
  exit "$code"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH" 2
}

require_docker() {
  require_cmd docker
  require_cmd curl
  require_cmd jq
  docker info >/dev/null 2>&1 ||
    die "Docker daemon is not reachable; start Docker Desktop/engine and retry" 2
  docker compose version >/dev/null 2>&1 ||
    die "the docker compose v2 plugin is required" 2
}

# resolve_compose_file KIND
#
# KIND is "demo" or "cluster". COMPOSE_FILE wins when set (a colon-separated
# list is accepted, as with Compose itself); otherwise the first existing
# candidate is used. For a cluster test a dedicated cluster file is preferred,
# but a single compose file with a `cluster` profile is accepted.
resolve_compose_file() {
  local kind=$1 f
  if [[ -n ${COMPOSE_FILE:-} ]]; then
    local -a files
    if ! IFS=':' read -r -a files <<< "$COMPOSE_FILE"; then
      die "COMPOSE_FILE is not a valid colon-separated path list: $COMPOSE_FILE" 2
    fi
    for f in "${files[@]}"; do
      [[ -f $f ]] || die "COMPOSE_FILE entry '$f' does not exist" 2
    done
    return 0
  fi

  local -a candidates=()
  if [[ $kind == cluster ]]; then
    candidates+=(
      docker-compose.cluster.yml
      docker-compose.cluster.yaml
      compose/docker-compose.cluster.yml
      compose/docker-compose.cluster.yaml
      compose/cluster.yml
      compose/cluster.yaml
    )
  fi
  candidates+=(
    docker-compose.yml
    docker-compose.yaml
    compose.yml
    compose.yaml
    compose/docker-compose.yml
    compose/docker-compose.yaml
  )

  for f in "${candidates[@]}"; do
    if [[ -f $REPO_ROOT/$f ]]; then
      COMPOSE_FILE=$f
      return 0
    fi
  done
  die "no compose file found (looked for: ${candidates[*]}); set COMPOSE_FILE" 2
}

# init_compose PROJECT PROFILE
#
# Builds the shared `compose` wrapper. The project name is explicit so a smoke
# run never touches a developer's default project, and `down -v` only removes
# volumes created by this run.
init_compose() {
  PROJECT=$1
  PROFILE=$2
  [[ -n ${COMPOSE_FILE:-} ]] || die "COMPOSE_FILE is not set" 2
  [[ -n $PROJECT ]] || die "PROJECT is empty" 2

  local -a files
  IFS=':' read -r -a files <<< "$COMPOSE_FILE"
  COMPOSE_FILE_ARGS=()
  local f
  for f in "${files[@]}"; do
    COMPOSE_FILE_ARGS+=(-f "$f")
  done
}

compose() {
  if [[ -n ${PROFILE:-} ]]; then
    docker compose "${COMPOSE_FILE_ARGS[@]}" \
      --project-directory "$REPO_ROOT" \
      --project-name "$PROJECT" \
      --profile "$PROFILE" \
      "$@"
  else
    docker compose "${COMPOSE_FILE_ARGS[@]}" \
      --project-directory "$REPO_ROOT" \
      --project-name "$PROJECT" \
      "$@"
  fi
}

load_compose_config() {
  CONFIG_JSON=$(compose config --format json) ||
    die "docker compose config failed for $COMPOSE_FILE" 2
  [[ -n $CONFIG_JSON ]] || die "docker compose config returned an empty document" 2
}

service_exists() {
  jq -e --arg s "$1" '.services[$s] != null' >/dev/null 2>&1 <<< "$CONFIG_JSON"
}

# service_command SERVICE returns the flattened entrypoint + command. Compose
# may render either as a string or a list; only the flags matter here.
service_command() {
  jq -r --arg s "$1" '
    def flat: if type == "array" then join(" ") elif type == "string" then . else "" end;
    (.services[$s] // {}) as $svc
    | (($svc.entrypoint // []) | flat) + " " + (($svc.command // []) | flat)
  ' <<< "$CONFIG_JSON" | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//'
}

# get_flag COMMAND FLAG prints the value of a Go-style flag (-name=value or
# -name value). The leading space matches only a whole flag, so -addr does not
# match -raft-addr.
get_flag() {
  local cmd=$1 name=$2 re value
  re="(^| )${name}=([^ ]+)"
  if [[ $cmd =~ $re ]]; then
    value=${BASH_REMATCH[2]}
  else
    re="(^| )${name}[ ]+([^ ]+)"
    if [[ $cmd =~ $re ]]; then
      value=${BASH_REMATCH[2]}
    else
      return 1
    fi
  fi
  value=${value%\"}; value=${value#\"}
  value=${value%\'}; value=${value#\'}
  printf '%s' "$value"
}

port_from_addr() {
  local addr=$1
  [[ -n $addr ]] || return 1
  if [[ $addr == *:* ]]; then
    printf '%s' "${addr##*:}"
  else
    printf '%s' "$addr"
  fi
}

# service_port SERVICE FLAG DEFAULT
# Reads the listen port from a Go-style listen flag (-addr, -raft-addr,
# -http-addr). Falls back to DEFAULT when the flag is absent or not numeric.
service_port() {
  local s=$1 flag=$2 default=$3 cmd value port
  cmd=$(service_command "$s")
  value=$(get_flag "$cmd" "$flag" || true)
  if port=$(port_from_addr "$value"); then
    if [[ $port =~ ^[0-9]+$ ]]; then
      printf '%s' "$port"
      return 0
    fi
  fi
  printf '%s' "$default"
}

# kv_service_addr SERVICE DEFAULT_PORT
# The address used in the range map. A v2 compose normally sets -id to the
# service DNS name (kv1:7001); use it when it is routable from peers. If -id is
# only a node name, or points at loopback, use SERVICE:raft-port so replicas can
# reach each other on the compose network.
kv_service_addr() {
  local s=$1 default_port=$2 cmd id port
  cmd=$(service_command "$s")
  id=$(get_flag "$cmd" "-id" || true)
  if [[ -n $id && $id == *:* ]]; then
    case "$id" in
      127.0.0.1:*|localhost:*|0.0.0.0:*|"[::]:"*) ;;
      *)
        printf '%s' "$id"
        return 0
        ;;
    esac
  fi
  port=$(service_port "$s" "-raft-addr" "$default_port")
  printf '%s:%s' "$s" "$port"
}

# classify_services fills META_SERVICES, KV_SERVICES, SQL_SERVICES and
# BOOTSTRAP_SERVICES from the compose model. v2 commands are recognised by
# their `-mode=` flag; the older v1 compose (db node/coordinator) is
# deliberately not classified and therefore fails the preflight.
classify_services() {
  META_SERVICES=()
  KV_SERVICES=()
  SQL_SERVICES=()
  BOOTSTRAP_SERVICES=()

  local s cmd
  while IFS= read -r s; do
    [[ -n $s ]] || continue
    cmd=$(service_command "$s")
    case "$cmd" in
      *"-mode=meta"*|*"-mode meta"*)
        META_SERVICES+=("$s")
        ;;
      *"-mode=node"*|*"-mode node"*)
        KV_SERVICES+=("$s")
        ;;
      *sqlsrv*)
        SQL_SERVICES+=("$s")
        ;;
      *init-ranges*)
        BOOTSTRAP_SERVICES+=("$s")
        ;;
      *)
        case "$s" in
          sql|sqlsrv|sql-server)
            SQL_SERVICES+=("$s")
            ;;
          *bootstrap*)
            BOOTSTRAP_SERVICES+=("$s")
            ;;
          meta|meta[0-9]*)
            META_SERVICES+=("$s")
            ;;
          kv|kv[0-9]*)
            KV_SERVICES+=("$s")
            ;;
        esac
        ;;
    esac
  done < <(jq -r '.services | keys[]' <<< "$CONFIG_JSON")
}

describe_services() {
  local s
  printf 'services in profile %s: ' "${PROFILE:-<none>}" >&2
  while IFS= read -r s; do
    printf '%s ' "$s" >&2
  done < <(jq -r '.services | keys[]' <<< "$CONFIG_JSON")
  printf '\n' >&2
}

container_id() {
  compose ps -q "$1" 2>/dev/null | head -n 1
}

container_ids() {
  compose ps -q "$1" 2>/dev/null || true
}

service_running_count() {
  local s=$1 cid n=0
  while IFS= read -r cid; do
    [[ -n $cid ]] || continue
    if [[ $(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null || printf 'false') == true ]]; then
      n=$((n + 1))
    fi
  done < <(container_ids "$s")
  printf '%s' "$n"
}

service_has_healthcheck() {
  local s=$1 cid
  cid=$(container_id "$s")
  [[ -n $cid ]] || return 1
  [[ $(docker inspect -f '{{if .Config.Healthcheck}}yes{{else}}no{{end}}' "$cid" 2>/dev/null || printf 'no') == yes ]]
}

wait_service_running() {
  local s=$1 deadline=$((SECONDS + TIMEOUT))
  while (( SECONDS < deadline )); do
    if (( $(service_running_count "$s") > 0 )); then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_service_healthy() {
  local s=$1 deadline=$((SECONDS + TIMEOUT)) cid health
  while (( SECONDS < deadline )); do
    cid=$(container_id "$s")
    if [[ -n $cid ]]; then
      health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid" 2>/dev/null || printf 'missing')
      case "$health" in
        healthy|none) return 0 ;;
      esac
    fi
    sleep 1
  done
  return 1
}

wait_for_log() {
  local s=$1 pattern=$2 deadline=$((SECONDS + TIMEOUT))
  while (( SECONDS < deadline )); do
    if compose logs --no-color --tail=200 "$s" 2>/dev/null | grep -qF -- "$pattern"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# wait_all_services_running EXPECTED SERVICE...
# Each named service must have at least one running container and the total
# must equal EXPECTED. Three named replicas are therefore not satisfied by one
# scaled service with three containers only if the caller passes one name and
# expects three; the smoke scripts pass one name per replica.
wait_all_services_running() {
  local expected=$1
  shift
  local -a names=("$@")
  local deadline=$((SECONDS + TIMEOUT)) total s running
  while (( SECONDS < deadline )); do
    total=0
    for s in "${names[@]}"; do
      running=$(service_running_count "$s")
      if (( running == 0 )); then
        total=-1
        break
      fi
      total=$((total + running))
    done
    if (( total == expected )); then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_meta_ready() {
  local deadline=$((SECONDS + TIMEOUT)) out="" leader=""
  local replicas=${META_REPLICA_COUNT:-0}
  if (( replicas <= 0 )); then
    replicas=${#META_SERVICES[@]}
  fi
  while (( SECONDS < deadline )); do
    if out=$(status_json 2>/dev/null); then
      leader=$(jq -r '.meta_leader // empty' <<< "$out" 2>/dev/null || true)
      if [[ -n $leader ]]; then
        META_LEADER=$leader
        return 0
      fi
      # A standalone meta (demo profile) is ready without a replicated leader
      # field; the compose bootstrap script documents this explicitly.
      if (( replicas <= 1 )); then
        META_LEADER=""
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

# meta_leader_addr returns the address data commands should use. A replicated
# meta refuses status on a follower, so status_json already retries until one
# answers; standalone meta has an empty leader and the probed address is fine.
meta_leader_addr() {
  local out leader
  out=$(status_json 2>/dev/null) || return 1
  leader=$(jq -r '.meta_leader // empty' <<< "$out" 2>/dev/null || true)
  if [[ -n $leader ]]; then
    printf '%s' "$leader"
  else
    printf '%s' "$META_ADDR"
  fi
}

wait_kv_ready() {
  local s=$1
  wait_service_running "$s" || return 1
  if service_has_healthcheck "$s"; then
    wait_service_healthy "$s" || return 1
  else
    wait_for_log "$s" "kv2 node started" || return 1
  fi
}

# kvctl_exec runs /app/kvctl inside a running container. The image is shared
# by meta, KV, SQL and bootstrap services, so any of them can host the client.
kvctl_exec() {
  local bin=${KVCTL_BIN:-/app/kvctl}
  if [[ -n ${KVCTL_ADMIN_TOKEN:-} ]]; then
    compose exec -T "$PROBE_SERVICE" "$bin" -admin-token "$KVCTL_ADMIN_TOKEN" "$@"
  else
    compose exec -T "$PROBE_SERVICE" "$bin" "$@"
  fi
}

# kvctl_meta retries across the meta replicas. A follower forwards a command
# to the leader, but a meta that is still starting refuses connections; trying
# the next address keeps the wait loop honest.
kvctl_meta() {
  local addr
  for addr in "${META_ADDRS[@]}"; do
    if kvctl_exec -meta "$addr" "$@" 2>/dev/null; then
      return 0
    fi
  done
  kvctl_exec -meta "${META_ADDRS[0]}" "$@"
}

ranges_json() {
  local addr out fallback=""
  for addr in "${META_ADDRS[@]}"; do
    if out=$(kvctl_exec -meta "$addr" -format json ranges 2>/dev/null); then
      if jq -e 'length > 0 and all(.[]; ((.replicas // []) | length) > 0)' \
        >/dev/null 2>&1 <<< "$out"; then
        printf '%s' "$out"
        return 0
      fi
      if [[ -z $fallback ]]; then
        fallback=$out
      fi
    fi
  done
  if [[ -n $fallback ]]; then
    printf '%s' "$fallback"
    return 0
  fi
  kvctl_exec -meta "${META_ADDRS[0]}" -format json ranges
}

status_json() {
  local addr out fallback="" leader=""
  for addr in "${META_ADDRS[@]}"; do
    if out=$(kvctl_exec -meta "$addr" -format json status 2>/dev/null); then
      leader=$(jq -r '.meta_leader // empty' <<< "$out" 2>/dev/null || true)
      if [[ -n $leader ]]; then
        printf '%s' "$out"
        return 0
      fi
      if [[ -z $fallback ]]; then
        fallback=$out
      fi
    fi
  done
  if [[ -n $fallback ]]; then
    printf '%s' "$fallback"
    return 0
  fi
  kvctl_exec -meta "${META_ADDRS[0]}" -format json status
}

ensure_ranges() {
  local json="" initialized=0

  if json=$(ranges_json 2>/dev/null); then
    initialized=$(jq '[.[] | select(((.replicas // []) | length) > 0)] | length' <<< "$json" 2>/dev/null || printf '0')
  fi
  if (( initialized > 0 )); then
    log "range map already initialized (${initialized} range(s) with replicas)"
    return 0
  fi

  if [[ ${USE_BOOTSTRAP_SERVICE:-0} == 1 && ${#BOOTSTRAP_SERVICES[@]} -gt 0 ]]; then
    log "bootstrapping range map via compose service '${BOOTSTRAP_SERVICES[0]}'"
    compose run --rm -T --no-deps "${BOOTSTRAP_SERVICES[0]}" || return 1
  else
    log "bootstrapping range map via kvctl init-ranges (replicas: $KV_NODES)"
    kvctl_meta init-ranges -replicas "$KV_NODES" || return 1
  fi

  # init-ranges verifies its own write, but the range map is read through the
  # meta leader; poll until every replica address is visible.
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if json=$(ranges_json 2>/dev/null) &&
      jq -e --argjson n "$KV_REPLICA_COUNT" \
        'length >= 1 and all(.[]; ((.replicas // []) | length) == $n)' \
        >/dev/null 2>&1 <<< "$json"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# range_map_ok JSON EXPECTED
#
# Every range must have exactly EXPECTED replicas. The meta range descriptor
# currently does not populate Leader (no UpdateLeader caller), so an empty
# leader is expected and must not fail the smoke test; when a leader is
# present it must be one of the replicas.
range_map_ok() {
  local json=$1 expected=$2
  jq -e --argjson n "$expected" '
    length >= 1 and
    all(.[]; (. as $r |
      ((($r.replicas // []) | length) == $n) and
      (($r.leader // "") == "" or ((($r.replicas // []) | index($r.leader)) != null))
    ))
  ' >/dev/null 2>&1 <<< "$json"
}

wait_range_map() {
  local expected=$1 deadline=$((SECONDS + TIMEOUT)) json=""
  while (( SECONDS < deadline )); do
    if json=$(ranges_json 2>/dev/null); then
      if range_map_ok "$json" "$expected"; then
        printf '%s' "$json"
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

kv_roundtrip() {
  local key="smoke:compose:$SMOKE_ID" value="kv-ok-$SMOKE_ID" got="" leader=""
  leader=$(meta_leader_addr) || return 1
  kvctl_exec -meta "$leader" -nodes "$KV_NODES" -format json put "$key" "$value" >/dev/null || return 1
  got=$(kvctl_exec -meta "$leader" -nodes "$KV_NODES" -format json get "$key" 2>/dev/null |
    jq -r '.value // empty' 2>/dev/null) || return 1
  [[ $got == "$value" ]]
}

wait_kv_roundtrip() {
  local deadline=$((SECONDS + TIMEOUT))
  while (( SECONDS < deadline )); do
    if kv_roundtrip; then
      return 0
    fi
    sleep 1
  done
  return 1
}

sql_endpoint() {
  if [[ $SQL_URL == */v1/sql ]]; then
    printf '%s' "$SQL_URL"
  else
    printf '%s/v1/sql' "${SQL_URL%/}"
  fi
}

sql_health_url() {
  if [[ $SQL_URL == */v1/sql ]]; then
    printf '%s/health' "${SQL_URL%/v1/sql}"
  else
    printf '%s/health' "${SQL_URL%/}"
  fi
}

wait_sql_health() {
  local deadline=$((SECONDS + TIMEOUT)) body="" url
  url=$(sql_health_url)
  local -a args=(-fsS --connect-timeout 2 --max-time 3)
  if [[ ${SQL_INSECURE:-0} == 1 ]]; then
    args+=(-k)
  fi
  while (( SECONDS < deadline )); do
    if body=$(curl "${args[@]}" "$url" 2>/dev/null); then
      if jq -e '.status == "ok"' >/dev/null 2>&1 <<< "$body"; then
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

# sql_exec SQL prints the JSON result on success and returns non-zero on a
# transport or HTTP error. The SQL error body is echoed to stderr so a failed
# smoke run shows the engine's message rather than a generic curl failure.
sql_exec() {
  local sql=$1 endpoint body http payload
  endpoint=$(sql_endpoint)
  body=$SMOKE_TMPDIR/sql-body.$$.json
  payload=$(jq -cn --arg sql "$sql" '{sql: $sql}') || return 1

  local -a args=(
    -sS
    --connect-timeout 3
    --max-time 20
    -o "$body"
    -w '%{http_code}'
    -X POST
    -H 'Content-Type: application/json'
  )
  if [[ -n ${SQL_TOKEN:-} ]]; then
    args+=(-H "Authorization: Bearer ${SQL_TOKEN}")
  fi
  if [[ ${SQL_INSECURE:-0} == 1 ]]; then
    args+=(-k)
  fi

  if ! http=$(curl "${args[@]}" --data-binary "$payload" "$endpoint" 2>"$SMOKE_TMPDIR/curl.err"); then
    warn "curl failed for $endpoint: $(tr '\n' ' ' < "$SMOKE_TMPDIR/curl.err")"
    rm -f "$body"
    return 1
  fi
  if [[ $http != 2* ]]; then
    warn "SQL endpoint returned HTTP $http for: $sql"
    if [[ -s $body ]]; then
      cat "$body" >&2
    fi
    rm -f "$body"
    return 1
  fi
  cat "$body"
  rm -f "$body"
}

run_sql() {
  local sql=$1 out
  if ! out=$(sql_exec "$sql"); then
    warn "SQL statement failed: $sql"
    return 1
  fi
  printf '%s' "$out"
}

assert_sql_value() {
  local sql=$1 expected=$2 out=""
  out=$(sql_exec "$sql") || return 1
  if ! jq -e --arg v "$expected" \
    '(.rows // [] | length) == 1 and .rows[0][0] == $v' \
    >/dev/null 2>&1 <<< "$out"; then
    warn "expected SQL value '$expected', got: $out"
    return 1
  fi
}

sql_wait_value() {
  local sql=$1 expected=$2
  local timeout=${3:-$TIMEOUT}
  local deadline=$((SECONDS + timeout)) out=""
  while (( SECONDS < deadline )); do
    if out=$(sql_exec "$sql" 2>/dev/null) &&
      jq -e --arg v "$expected" \
        '(.rows // [] | length) == 1 and .rows[0][0] == $v' \
        >/dev/null 2>&1 <<< "$out"; then
      return 0
    fi
    sleep 1
  done
  warn "timed out waiting for SQL value '$expected'; last output: ${out:-<none>}"
  return 1
}

# sql_write_then_wait_value INSERT_SQL SELECT_SQL EXPECTED
#
# Used after a KV is stopped: the INSERT may race leader election, and a
# successful INSERT must not be repeated (that would produce a duplicate-key
# error). Once the write succeeds, only the read is retried.
sql_write_then_wait_value() {
  local insert=$1 select=$2 expected=$3 timeout=${4:-$TIMEOUT}
  local deadline=$((SECONDS + timeout)) inserted=0 out="" lower
  while (( SECONDS < deadline )); do
    if (( inserted == 0 )); then
      if out=$(sql_exec "$insert" 2>&1); then
        inserted=1
      else
        lower=$(printf '%s' "$out" | tr '[:upper:]' '[:lower:]')
        case "$lower" in
          *duplicate*|*"already exists"*|*"table exists"*) inserted=1 ;;
        esac
      fi
    fi
    if (( inserted == 1 )); then
      if out=$(sql_exec "$select" 2>/dev/null) &&
        jq -e --arg v "$expected" \
          '(.rows // [] | length) == 1 and .rows[0][0] == $v' \
          >/dev/null 2>&1 <<< "$out"; then
        return 0
      fi
    fi
    sleep 1
  done
  warn "timed out writing/reading '$expected' after the KV change; last output: ${out:-<none>}"
  return 1
}

dump_diagnostics() {
  local s
  warn "--- docker compose ps ---"
  compose ps 2>&1 | sed 's/^/  /' >&2 || true
  for s in "$@"; do
    warn "--- logs: $s (tail 60) ---"
    compose logs --no-color --tail=60 "$s" 2>&1 | sed 's/^/  /' >&2 || true
  done
}

# run_with_timeout SECONDS COMMAND... bounds a command that would otherwise
# hang (for example `docker compose up` waiting on a dependency). Returns 124
# on timeout; the cleanup trap still removes partially started containers.
run_with_timeout() {
  local seconds=$1
  shift
  "$@" &
  local pid=$!
  local deadline=$((SECONDS + seconds))
  while kill -0 "$pid" 2>/dev/null; do
    if (( SECONDS >= deadline )); then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 1
  done
  wait "$pid"
}

cleanup_project() {
  if [[ ${KEEP:-0} == 1 ]]; then
    warn "KEEP=1: leaving project '$PROJECT' running; remove it with:"
    warn "  docker compose -f ${COMPOSE_FILE:-<compose-file>} --project-name ${PROJECT:-<project>} --profile ${PROFILE:-<profile>} down -v"
  elif [[ ${STACK_STARTED:-0} == 1 ]]; then
    log "cleaning up project '$PROJECT' (docker compose down -v)"
    if ! compose down -v --remove-orphans --timeout 15 >/dev/null 2>&1; then
      # A volume can still be referenced for a moment while the last
      # container is stopping; one bounded retry avoids leaving debris.
      sleep 2
      if ! compose down -v --remove-orphans --timeout 15 >/dev/null 2>&1; then
        warn "docker compose down failed; containers/volumes may remain in project '$PROJECT'"
      fi
    fi
  fi
  rm -rf "$SMOKE_TMPDIR"
}
