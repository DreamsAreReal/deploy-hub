#!/bin/bash
# deploy-hub runner — the single SSH entry point on the VPS.
#
# Installed as the forced command of the `deploy` user
# (authorized_keys: command="/opt/deploy-hub/bin/runner.sh",no-pty,...),
# so whatever the client asks for lands in $SSH_ORIGINAL_COMMAND and is
# dispatched here. Anything that is not a known verb is refused.
#
# Verbs:
#   deploy <app> <tag>     pull image, restart the app container, health-gate,
#                          rollback on failure; metadata comes on stdin
#   rollback <app> [tag]   no tag: roll back to the previous sha (journaled as
#                          `rollback`); explicit tag: redeploy that version
#                          (journaled as `redeploy` — it may roll forward)
#   status                 one line per app: app | sha | health | last deploy
#   history <app>          last journal lines of the app
#
# stdin protocol for `deploy` (key=value lines; keeps the ephemeral
# GITHUB_TOKEN out of argv and logs):
#   token=<GITHUB_TOKEN>   required for GHCR pull (docker login -> pull -> logout)
#   actor=<github login>   docker login username
#   subject=<commit line>  first line of the commit message, for the card
#   start=<epoch seconds>  workflow start, for full push->healthy duration
#
# App registry: /opt/deploy-hub/apps.list, one app per line: `<app> <dir>`
# (dir defaults to /opt/<app>; the pilot lives in /opt/portfolio-new).
# Per-app config: <dir>/app.conf with profile=, port=, health_path=, image=.
set -euo pipefail

HUB_DIR="${HUB_DIR:-/opt/deploy-hub}"   # overridable for tests
# docker compose stats the cwd even with -f: a manual `sudo -u deploy` run
# from /root would die on permissions, so normalize the cwd first
cd "${HUB_DIR}" 2>/dev/null || cd /
APPS_LIST="$HUB_DIR/apps.list"
LOG_FILE="$HUB_DIR/deploys.log"
LOCK_FILE="$HUB_DIR/deploy.lock"
TELEGRAM_ENV="$HUB_DIR/telegram.env"   # optional: TG_TOKEN=, TG_CHAT_ID= (600, owner deploy)
TELEGRAM_LOG="$HUB_DIR/telegram.log"
SMOKE_CONF="$HUB_DIR/smoke.conf"       # optional: TCP ports to probe after each op; empty/absent = skip
REGISTRY=ghcr.io
HEALTH_TIMEOUT=90      # seconds; contract: never below 60 (guardrail from the brief)
HEALTH_INTERVAL=5
LOCK_WAIT=300          # a deploy behind a slow one waits up to 5 min, then fails honestly

# --- user-facing strings (en) -------------------------------------------------
S_REFUSED="refused: only 'deploy <app> <tag>', 'rollback <app> [tag]', 'status', 'history <app>' are accepted"
S_UNKNOWN_APP="refused: app not in allowlist"
S_BAD_TAG="refused: tag must match ^sha-[0-9a-f]{7,40}\$"
S_NO_CONF="error: app.conf not found for app"
S_LOCK_BUSY="error: another deploy holds the lock (waited ${LOCK_WAIT}s, giving up)"
S_LOCK_WAIT="waiting for deploy lock (another deploy in progress)..."
S_PULL_FAIL="error: image pull failed"
S_HEALTH_OK="health: ok"
S_HEALTH_FAIL="health: no HTTP 200 within ${HEALTH_TIMEOUT}s"
S_ROLLED_BACK="rolled back to previous sha"
S_REDEPLOYED="redeployed to requested sha"
S_FIRST_FAIL="first deploy failed, app stopped (no rollback target)"
S_CARD_FIRST_FAIL_HINT="Check /opt/%s/.env and app logs, then push a fix"
S_CARD_FAIL_HINT="Run: ssh vpn 'docker logs %s --tail 50'"

log_line() { # log_line <app> <sha7> <action> <result> <duration_s>
  # container operations carry a post-op smoke suffix: whatever critical service
  # must survive a deploy is probed right after, so every deploy/rollback/stop
  # line records `vpn=ok|skip|fail` (skip = no smoke ports configured)
  local suffix=""
  case "$3" in deploy|rollback|redeploy|stop) suffix=" vpn=$(vpn_smoke)" ;; esac
  printf '[%s] %s@%s %s %s %ss%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "$3" "$4" "$5" "$suffix" >> "$LOG_FILE"
}

smoke_ports() { # configured post-op TCP smoke ports, one per line in smoke.conf
  # (comments and blank lines ignored). No file / no ports => nothing to probe.
  [ -f "$SMOKE_CONF" ] || return 0
  grep -vE '^[[:space:]]*(#|$)' "$SMOKE_CONF" 2>/dev/null | tr -s ' \t' '\n' \
    | grep -E '^[0-9]{1,5}$' || true
}

vpn_smoke() { # post-op probe of the configured smoke ports (default: none).
  # Ports live in smoke.conf so removing a service (e.g. the VPN) does not leave
  # the journal permanently reading `vpn=fail`: with nothing configured there is
  # nothing to prove, so the result is `skip`, not `fail`.
  local ports; ports=$(smoke_ports)
  [ -n "$ports" ] || { echo skip; return; }
  local p
  for p in $ports; do
    nc -z -w 2 127.0.0.1 "$p" 2>/dev/null || { echo fail; return; }
  done
  echo ok
}

fmt_duration() { # seconds -> "6m12s" | "42s"
  local s=$1
  if (( s >= 60 )); then printf '%dm%02ds' $((s / 60)) $((s % 60)); else printf '%ds' "$s"; fi
}

# --- Telegram card (signature element) ----------------------------------------
# Single render point: tournament winner "system pulse", 3 lines:
#   1. status emoji + app + outcome
#   2. sha7 • commit subject • duration
#   3. pulse (deploy #N • M days stable) or next action on failure
# Pulse counters come ONLY from deploys.log.
render_card() { # render_card <kind> <app> <sha7> <duration> <subject> <extra> [reason]
  local kind=$1 app=$2 sha7=$3 duration=$4 subject=$5 extra=$6 reason=${7:-}
  local line1 line2 line3 cname
  cname=$(app_container "$app"); cname=${cname:-$app}   # fallback: container may be gone
  line2="${sha7} • ${subject:-no commit subject} • ${duration}"
  case "$kind" in
    ok)
      line1="✅ ${app} live"
      line3="$(render_pulse "$app")"
      ;;
    rollback)
      line1="⏪ ${app} rolled back to ${extra}${reason:+ — ${reason}}"
      line3="$(printf "$S_CARD_FAIL_HINT" "$cname")"
      ;;
    rollback-fail)
      line1="❌ ${app} health failed${reason:+ (${reason})} and rollback to ${extra} failed too"
      line3="$(printf "$S_CARD_FAIL_HINT" "$cname")"
      ;;
    redeploy)
      # explicit-tag operation: may move the app FORWARD, so it must not be
      # worded as a rollback (evaluator M3+M4)
      line1="✅ ${app} redeployed to ${extra}"
      line3="$(render_pulse "$app")"
      ;;
    redeploy-fail)
      line1="❌ ${app} redeploy to ${extra} failed${reason:+ (${reason})}"
      line3="$(printf "$S_CARD_FAIL_HINT" "$cname")"
      ;;
    first-fail)
      line1="❌ ${app} first deploy failed — app stopped${reason:+ — ${reason}}"
      line3="$(printf "$S_CARD_FIRST_FAIL_HINT" "$(basename "$(app_dir "$app")")")"
      ;;
  esac
  printf '%s\n%s\n%s' "$line1" "$line2" "$line3"
}

render_pulse() { # deploy counter + days since last fail/rollback, from deploys.log only
  local app=$1 n last_bad_ts days
  # count successful deploys only: "Deploy #5" must not include failed attempts
  n=$(grep -c "] ${app}@[0-9a-f]* deploy ok " "$LOG_FILE" 2>/dev/null || true)
  if (( n <= 1 )); then printf 'First deploy'; return; fi
  last_bad_ts=$(grep -E "] ${app}@[0-9a-f]+ (deploy|rollback) (fail|ok)" "$LOG_FILE" \
    | grep -E ' (deploy fail|rollback ok)' | tail -1 | sed -E 's/^\[([^]]+)\].*/\1/' || true)
  if [ -z "$last_bad_ts" ]; then
    # no incident on record: stable since the first deploy
    last_bad_ts=$(head -1 "$LOG_FILE" | sed -E 's/^\[([^]]+)\].*/\1/')
  fi
  days=$(( ($(date -u +%s) - $(date -u -d "$last_bad_ts" +%s 2>/dev/null || date -u +%s)) / 86400 ))
  printf 'Deploy #%d • %d days stable' "$n" "$days"
}

send_card() { # send_card <app> <text>; logs `notify ok|skip|fail` to the journal
  local app=$1 text=$2 resp
  local TG_TOKEN="" TG_CHAT_ID=""
  if [ -f "$TELEGRAM_ENV" ]; then
    TG_TOKEN=$(awk -F= '$1=="TG_TOKEN"{print substr($0, index($0,"=")+1)}' "$TELEGRAM_ENV")
    TG_CHAT_ID=$(awk -F= '$1=="TG_CHAT_ID"{print substr($0, index($0,"=")+1)}' "$TELEGRAM_ENV")
  fi
  if [ -z "$TG_TOKEN" ] || [ -z "$TG_CHAT_ID" ]; then
    # no credentials yet: keep the rendered card on record so the exact text
    # of every card is verifiable even before delivery is enabled
    printf '[%s] %s skipped (no telegram.env), rendered card:\n%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$app" "$text" >> "$TELEGRAM_LOG"
    log_line "$app" "-" notify skip 0
    return 0
  fi
  resp=$(curl -sS -m 10 "https://api.telegram.org/bot${TG_TOKEN}/sendMessage" \
    --data-urlencode "chat_id=${TG_CHAT_ID}" \
    --data-urlencode "text=${text}" 2>&1) || true
  printf '[%s] %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$app" "$resp" >> "$TELEGRAM_LOG"
  if printf '%s' "$resp" | grep -q '"ok":true'; then
    log_line "$app" "-" notify ok 0
  else
    log_line "$app" "-" notify fail 0
  fi
}

# --- app registry --------------------------------------------------------------
app_dir() { awk -v a="$1" '$1==a {print ($2 != "" ? $2 : "/opt/" a); exit}' "$APPS_LIST"; }

conf_get() { # conf_get <dir> <key>
  awk -F= -v k="$2" '$1==k {print substr($0, index($0,"=")+1); exit}' "$1/app.conf"
}

app_container() { # container name = compose service `app` in the app dir
  local dir; dir=$(app_dir "$1")
  # DEPLOY_IMAGE placeholder: `ps` only needs the file to parse; without it
  # compose aborts on the :? guard (and pipefail would hit set -e -> || true)
  DEPLOY_IMAGE="${DEPLOY_IMAGE:-placeholder}" \
    docker compose -f "$dir/docker-compose.yml" ps -a --format '{{.Name}}' app 2>/dev/null | head -1 || true
}

state_get() { # state_get <dir> <key>  (.deploy-state: current=, previous=)
  [ -f "$1/.deploy-state" ] || return 0
  awk -F= -v k="$2" '$1==k {print $2; exit}' "$1/.deploy-state"
}

state_set() { # state_set <dir> <current> <previous>
  printf 'current=%s\nprevious=%s\n' "$2" "$3" > "$1/.deploy-state"
}

# --- health gate ---------------------------------------------------------------
health_gate() { # health_gate <profile> <port> <health_path>; 0=healthy
  local profile=$1 port=$2 path=$3 elapsed=0
  while (( elapsed < HEALTH_TIMEOUT )); do
    case "$profile" in
      static|service)
        # -s without -S: transient connect errors during container restart are
        # expected and used to read like failures in the output (consumer M3)
        if curl -fs -m 5 -o /dev/null "http://127.0.0.1:${port}${path}" 2>/dev/null; then return 0; fi
        ;;
      bot)
        # bot profile: rely on the compose healthcheck (functional getMe check, F4)
        local cname; cname=$(app_container "$CURRENT_APP")
        local hs; hs=$(docker inspect --format '{{.State.Health.Status}}' "$cname" 2>/dev/null || echo none)
        if [ "$hs" = healthy ]; then return 0; fi
        ;;
    esac
    sleep "$HEALTH_INTERVAL"
    elapsed=$((elapsed + HEALTH_INTERVAL))
  done
  return 1
}

acquire_lock() { # serialize deploys: one at a time on this 1-CPU box.
  # Announce a non-empty wait (a silent waiter reads like a hang and gets
  # killed mid-deploy) and fail honestly after LOCK_WAIT: an orphaned waiter
  # must not fire a stale operation long after the caller is gone.
  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    echo "$S_LOCK_WAIT"
    flock -w "$LOCK_WAIT" 9 || { echo "$S_LOCK_BUSY" >&2; exit 1; }
  fi
}

# --- deploy --------------------------------------------------------------------
compose_up() { # compose_up <dir> <image:tag>
  DEPLOY_IMAGE="$2" docker compose -f "$1/docker-compose.yml" up -d --pull never app
}

cmd_deploy() {
  local app=$1 tag=$2
  CURRENT_APP=$app
  local dir; dir=$(app_dir "$app")
  [ -n "$dir" ] && [ -f "$dir/app.conf" ] || { echo "$S_NO_CONF ($app)" >&2; exit 1; }

  local profile port path image
  profile=$(conf_get "$dir" profile)
  port=$(conf_get "$dir" port)
  path=$(conf_get "$dir" health_path)
  image=$(conf_get "$dir" image)

  # stdin protocol (see header). Token is used once for docker login and never echoed.
  local token="" actor="" subject="" start="" line
  while IFS= read -r line; do
    case "$line" in
      token=*)   token=${line#token=} ;;
      actor=*)   actor=${line#actor=} ;;
      subject=*) subject=${line#subject=} ;;
      start=*)   start=${line#start=} ;;
    esac
  done
  subject=$(printf '%s' "$subject" | tr -d '\000-\037' | cut -c1-120)
  echo "$start" | grep -Eq '^[0-9]{0,12}$' || start=""

  local sha7; sha7=$(printf '%s' "${tag#sha-}" | cut -c1-7)
  local t0; t0=$(date -u +%s)

  acquire_lock

  # ephemeral GHCR auth: login -> pull -> logout, logout guaranteed by trap
  if [ -n "$token" ]; then
    printf '%s' "$token" | docker login "$REGISTRY" -u "${actor:-x}" --password-stdin >/dev/null
    trap 'docker logout "$REGISTRY" >/dev/null 2>&1 || true' EXIT
  fi
  echo "pulling ${image}:${tag}"
  local pull_out
  if ! pull_out=$(docker pull -q "${image}:${tag}" 2>&1); then
    # keep the error on the server too: CI logs are not always reachable
    printf '[%s] %s pull error: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$app" "$pull_out" >> "$HUB_DIR/last-error.log"
    log_line "$app" "$sha7" deploy fail "$(( $(date -u +%s) - t0 ))"
    echo "$S_PULL_FAIL: $pull_out" >&2
    exit 1
  fi
  docker logout "$REGISTRY" >/dev/null 2>&1 || true

  local prev; prev=$(state_get "$dir" current)
  echo "starting ${app} @ ${tag}"
  compose_up "$dir" "${image}:${tag}"

  local t_end duration_s duration
  if health_gate "$profile" "$port" "$path"; then
    t_end=$(date -u +%s)
    duration_s=$(( t_end - ${start:-$t0} ))
    duration=$(fmt_duration "$duration_s")
    # idempotent redeploy of the same sha must NOT move the rollback target
    # (an evaluator rerun once collapsed current==previous, losing the target)
    if [ "${prev:-}" != "$tag" ]; then
      state_set "$dir" "$tag" "${prev:-}"
    fi
    log_line "$app" "$sha7" deploy ok "$(( t_end - t0 ))"
    echo "$S_HEALTH_OK — ${app}@${sha7} live (${duration})"
    send_card "$app" "$(render_card ok "$app" "$sha7" "$duration" "$subject" "")"
    return 0
  fi

  # health gate failed
  t_end=$(date -u +%s)
  duration=$(fmt_duration $(( t_end - ${start:-$t0} )))
  local reason
  case "$profile" in
    bot) reason="container unhealthy (functional healthcheck)" ;;
    *)   reason="no HTTP 200 on :${port}${path} within ${HEALTH_TIMEOUT}s" ;;
  esac
  log_line "$app" "$sha7" deploy fail "$(( t_end - t0 ))"
  echo "$S_HEALTH_FAIL" >&2

  if [ -n "$prev" ]; then
    local prev7; prev7=$(printf '%s' "${prev#sha-}" | cut -c1-7)
    echo "rolling back to ${prev}" >&2
    compose_up "$dir" "${image}:${prev}"
    if health_gate "$profile" "$port" "$path"; then
      log_line "$app" "$prev7" rollback ok "$(( $(date -u +%s) - t_end ))"
      echo "$S_ROLLED_BACK (${prev7})" >&2
      send_card "$app" "$(render_card rollback "$app" "$sha7" "$duration" "$subject" "$prev7" "$reason")"
    else
      log_line "$app" "$prev7" rollback fail "$(( $(date -u +%s) - t_end ))"
      send_card "$app" "$(render_card rollback-fail "$app" "$sha7" "$duration" "$subject" "$prev7" "$reason")"
    fi
  else
    # DEPLOY_IMAGE must be set even for `stop`: compose refuses to parse the
    # file otherwise and the app would silently keep running (caught in F4)
    if DEPLOY_IMAGE="${image}:${tag}" docker compose -f "$dir/docker-compose.yml" stop app >/dev/null 2>&1; then
      log_line "$app" "$sha7" stop ok 0
    else
      log_line "$app" "$sha7" stop fail 0
    fi
    echo "$S_FIRST_FAIL" >&2
    send_card "$app" "$(render_card first-fail "$app" "$sha7" "$duration" "$subject" "" "$reason")"
  fi
  exit 1
}

cmd_rollback() {
  local app=$1 tag=${2:-}
  # explicit tag = the operator asks for a concrete version: that is a
  # redeploy (it may roll FORWARD), not a rollback; журнал/card must not lie
  local verb=rollback
  [ -n "$tag" ] && verb=redeploy
  CURRENT_APP=$app
  local dir; dir=$(app_dir "$app")
  [ -n "$dir" ] && [ -f "$dir/app.conf" ] || { echo "$S_NO_CONF ($app)" >&2; exit 1; }
  local image profile port path
  image=$(conf_get "$dir" image)
  profile=$(conf_get "$dir" profile)
  port=$(conf_get "$dir" port)
  path=$(conf_get "$dir" health_path)
  [ -n "$tag" ] || tag=$(state_get "$dir" previous)
  [ -n "$tag" ] || { echo "error: no previous sha recorded for $app" >&2; exit 1; }
  local sha7; sha7=$(printf '%s' "${tag#sha-}" | cut -c1-7)
  local t0; t0=$(date -u +%s)
  acquire_lock
  compose_up "$dir" "${image}:${tag}"
  echo "waiting for the health gate..."
  local dur
  if health_gate "$profile" "$port" "$path"; then
    local cur; cur=$(state_get "$dir" current)
    if [ "${cur:-}" != "$tag" ]; then
      state_set "$dir" "$tag" "${cur:-}"
    fi
    dur=$(( $(date -u +%s) - t0 ))
    log_line "$app" "$sha7" "$verb" ok "$dur"
    if [ "$verb" = redeploy ]; then echo "$S_REDEPLOYED (${sha7})"; else echo "$S_ROLLED_BACK (${sha7})"; fi
    # a manual rollback/redeploy is a state change like any deploy: without a
    # card the last card in the chat keeps claiming the old version is live
    send_card "$app" "$(render_card "$verb" "$app" "$sha7" "$(fmt_duration "$dur")" "manual $verb" "$sha7" "")"
  else
    dur=$(( $(date -u +%s) - t0 ))
    log_line "$app" "$sha7" "$verb" fail "$dur"
    echo "error: $verb target is not healthy" >&2
    send_card "$app" "$(render_card "$verb-fail" "$app" "$sha7" "$(fmt_duration "$dur")" "manual $verb" "$sha7" "target unhealthy")"
    exit 1
  fi
}

cmd_status() {
  local app dir_raw dir cur cur7 cname state hs last profile port path
  printf '%s | %s | %s | %s\n' app sha health "last deploy"
  while read -r app dir_raw; do
    [ -n "$app" ] || continue
    case "$app" in \#*) continue ;; esac
    dir=${dir_raw:-/opt/$app}
    cur=$(state_get "$dir" current); cur=${cur:-none}
    cur7=$(printf '%s' "${cur#sha-}" | cut -c1-7)   # journal-style sha7, less noise
    cname=$(app_container "$app")
    if [ -n "$cname" ]; then
      state=$(docker inspect --format '{{.State.Status}}' "$cname" 2>/dev/null || echo missing)
      hs=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}' "$cname" 2>/dev/null || echo -)
      # no container healthcheck: probe the gate URL live so the column is
      # never blank for static/service apps
      if [ "$hs" = "-" ] && [ "$state" = running ] && [ -f "$dir/app.conf" ]; then
        profile=$(conf_get "$dir" profile)
        case "$profile" in
          static|service)
            port=$(conf_get "$dir" port); path=$(conf_get "$dir" health_path)
            if curl -fsS -m 3 -o /dev/null "http://127.0.0.1:${port}${path}" 2>/dev/null; then
              hs=ok
            else
              hs=fail
            fi
            ;;
        esac
      fi
    else
      state=missing; hs=-
    fi
    # last OPERATION, not just deploy: a rollback moves the app too (consumer M3)
    last=$(grep "] ${app}@" "$LOG_FILE" 2>/dev/null | grep -E ' (deploy|rollback|redeploy|stop) ' | tail -1 | sed -E 's/^\[([^]]+)\].*/\1/' || true)
    printf '%s | %s | %s/%s | %s\n' "$app" "$cur7" "$state" "$hs" "${last:-never}"
  done < "$APPS_LIST"
}

cmd_history() { # last journal lines of one app, over the deploy SSH channel
  local app=$1 n=${2:-20} out
  out=$(grep -F "] ${app}@" "$LOG_FILE" 2>/dev/null | tail -n "$n" || true)
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
  else
    echo "no journal entries for $app"
  fi
}

# --- dispatcher ----------------------------------------------------------------
main() {
  # forced command: real request lives in SSH_ORIGINAL_COMMAND;
  # direct invocation (root shell / RUNBOOK) passes argv instead
  local raw="${SSH_ORIGINAL_COMMAND:-$*}"
  # read -a: split words without pathname expansion (raw is attacker-reachable)
  local argv=()
  read -r -a argv <<< "$raw" || true
  local verb="${argv[0]:-}"
  case "$verb" in
    history)
      local app="${argv[1]:-}"
      printf '%s' "$app" | grep -Eq '^[a-z0-9][a-z0-9._-]{0,40}$' \
        || { echo "$S_UNKNOWN_APP" >&2; log_refuse "$raw"; exit 1; }
      [ -n "$(app_dir "$app")" ] || { echo "$S_UNKNOWN_APP" >&2; log_refuse "$raw"; exit 1; }
      cmd_history "$app"
      ;;
    deploy|rollback)
      local app="${argv[1]:-}" tag="${argv[2]:-}"
      # allowlist first: unknown names are refused before anything runs
      printf '%s' "$app" | grep -Eq '^[a-z0-9][a-z0-9._-]{0,40}$' \
        || { echo "$S_UNKNOWN_APP" >&2; log_refuse "$raw"; exit 1; }
      [ -n "$(app_dir "$app")" ] || { echo "$S_UNKNOWN_APP" >&2; log_refuse "$raw"; exit 1; }
      if [ "$verb" = deploy ]; then
        printf '%s' "$tag" | grep -Eq '^sha-[0-9a-f]{7,40}$' \
          || { echo "$S_BAD_TAG" >&2; log_refuse "$raw"; exit 1; }
        cmd_deploy "$app" "$tag"
      else
        if [ -n "$tag" ]; then
          printf '%s' "$tag" | grep -Eq '^sha-[0-9a-f]{7,40}$' \
            || { echo "$S_BAD_TAG" >&2; log_refuse "$raw"; exit 1; }
        fi
        cmd_rollback "$app" "$tag"
      fi
      ;;
    status)
      cmd_status
      ;;
    *)
      echo "$S_REFUSED" >&2
      log_refuse "$raw"
      exit 1
      ;;
  esac
}

log_refuse() { # journal a refused request; keep only a sanitized fingerprint
  # journal format is space-separated: squash spaces so the line stays parseable
  local what; what=$(printf '%s' "$1" | tr ' ' '_' | tr -cd 'a-zA-Z0-9._-' | cut -c1-40)
  log_line "-" "-" refuse "denied(${what:-empty})" 0
}

main "$@"
