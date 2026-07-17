#!/bin/bash
# deploy-hub prune — keep the RUNNING image plus 2 previous versions per app,
# delete the rest of the app's tags, then drop dangling layers.
#
# "Previous" is counted from the RUNNING version (.deploy-state), not by image
# date: after a rollback the newest-by-date tag is the broken one, and pruning
# by date would delete the good predecessor (brief guardrail: a rollback
# target must survive pruning).
#
# Runs as root from a systemd timer (deploy-hub-prune.timer, daily); can be
# run by hand any time: /opt/deploy-hub/bin/prune.sh
set -euo pipefail

HUB_DIR="${HUB_DIR:-/opt/deploy-hub}"
APPS_LIST="$HUB_DIR/apps.list"
LOG_FILE="$HUB_DIR/deploys.log"
KEEP_PREVIOUS=2

log_prune() { # log_prune <app> <sha7> <result> <detail>
  printf '[%s] %s@%s prune %s %ss %s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "$3" "$4" "$5" >> "$LOG_FILE"
}

state_get() { awk -F= -v k="$2" '$1==k {print $2; exit}' "$1/.deploy-state" 2>/dev/null || true; }
conf_get()  { awk -F= -v k="$2" '$1==k {print substr($0, index($0,"=")+1); exit}' "$1/app.conf" 2>/dev/null || true; }

while read -r app dir_raw; do
  [ -n "$app" ] || continue
  case "$app" in \#*) continue ;; esac
  dir=${dir_raw:-/opt/$app}
  image=$(conf_get "$dir" image)
  [ -n "$image" ] || continue
  t0=$(date +%s)

  current=$(state_get "$dir" current)
  previous=$(state_get "$dir" previous)
  [ -n "$current" ] || { log_prune "$app" "-" skip 0 "no-deploy-state"; continue; }

  # keep set: running + up to KEEP_PREVIOUS distinct predecessors, counted
  # back from the running one via .deploy-state and the journal history
  keep="$current"
  [ -n "$previous" ] && [ "$previous" != "$current" ] && keep="$keep $previous"
  # journal history (oldest->newest ok-tags), walk backwards for one more
  while read -r sha7; do
    n_kept=$(wc -w <<< "$keep")
    (( n_kept >= 1 + KEEP_PREVIOUS )) && break
    case " $keep " in *" sha-${sha7}"*|*"${sha7}"*) continue ;; esac
    # expand sha7 to the full tag if such an image exists
    full=$(docker images "$image" --format '{{.Tag}}' | grep -E "^sha-${sha7}" | head -1 || true)
    [ -n "$full" ] && keep="$keep $full"
  done < <(grep -E "] ${app}@[0-9a-f]+ (deploy|rollback|redeploy) ok " "$LOG_FILE" 2>/dev/null \
           | sed -E 's/^\[[^]]+\] [^@]+@([0-9a-f]+) .*/\1/' | tac | awk '!seen[$0]++')

  removed=0
  while read -r tag; do
    [ "$tag" = latest ] && continue          # weightless alias of the newest build
    case " $keep " in *" $tag "*) continue ;; esac
    docker rmi "$image:$tag" >/dev/null 2>&1 && removed=$((removed+1)) || true
  done < <(docker images "$image" --format '{{.Tag}}')

  cur7=$(printf '%s' "${current#sha-}" | cut -c1-7)
  log_prune "$app" "$cur7" ok "$(( $(date +%s) - t0 ))" "removed=${removed} kept=$(wc -w <<< "$keep")"
done < "$APPS_LIST"

docker image prune -f >/dev/null 2>&1 || true
