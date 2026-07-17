#!/bin/bash
# wb4-provision.sh — the ONE privileged server-side step of the bot's "Connect"
# flow (WB4). The bot runs as user `deploy` (no sudo, cannot write /opt or
# apps.list, both root-owned), so this helper does exactly the root-only work:
#
#   * create /opt/<app>/ (docker-compose.yml from the template, app.conf, .env)
#   * add the `<app> <dir>` line to /opt/deploy-hub/apps.list
#   * refresh Caddy routes (caddy-sync)
#
# It is invoked ONLY through a narrow sudoers rule:
#   deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/wb4-provision.sh
# and every argument is validated against a strict regex here, so there is no
# arbitrary-shell surface even though the caller is unprivileged. It performs no
# GitHub actions and reads no secrets — the bot does the GitHub side itself.
#
#   wb4-provision.sh <app> <profile> <port> <cport> <mem> <health_path> <image>
#
# Idempotent: existing files/lines are left as-is.
set -euo pipefail

HUB_DIR=/opt/deploy-hub
APPS_LIST="$HUB_DIR/apps.list"
TEMPLATE="$HUB_DIR/server/compose-template.yml"
# fall back to the copy shipped next to this script if server/ is not mirrored
[ -f "$TEMPLATE" ] || TEMPLATE="$(cd "$(dirname "$0")/.." && pwd)/compose-template.yml"
[ -f "$TEMPLATE" ] || TEMPLATE="$HUB_DIR/compose-template.yml"

app=${1:-} profile=${2:-} port=${3:-} cport=${4:-} mem=${5:-} hpath=${6:-} image=${7:-}

# --- strict validation (attacker-reachable via the unprivileged caller) -------
die() { echo "wb4-provision: $1" >&2; exit 2; }
printf '%s' "$app"    | grep -Eq '^[a-z0-9][a-z0-9._-]{0,40}$'      || die "bad app"
case "$profile" in static|service|bot) ;; *) die "bad profile" ;; esac
[ -z "$port" ]  || printf '%s' "$port"  | grep -Eq '^[0-9]{2,5}$'   || die "bad port"
[ -z "$cport" ] || printf '%s' "$cport" | grep -Eq '^[0-9]{2,5}$'   || die "bad cport"
printf '%s' "$mem"    | grep -Eq '^[0-9]{1,5}[mg]$'                  || die "bad mem"
printf '%s' "$hpath"  | grep -Eq '^/[A-Za-z0-9._/-]{0,60}$'         || die "bad health_path"
printf '%s' "$image"  | grep -Eq '^ghcr\.io/[a-z0-9._/-]{1,100}$'   || die "bad image"
[ -f "$TEMPLATE" ] || die "compose template not found"

dir="/opt/$app"

if [ ! -d "$dir" ]; then
  install -d -m 755 -o deploy -g deploy "$dir"
  echo "created $dir"
fi

if [ ! -f "$dir/docker-compose.yml" ]; then
  cp "$TEMPLATE" "$dir/docker-compose.yml"
  sed -i "s|__NAME__|$app|; s|__MEM_LIMIT__|$mem|" "$dir/docker-compose.yml"
  if [ "$profile" = bot ]; then
    sed -i '/^    ports:/,+1d' "$dir/docker-compose.yml"
  else
    sed -i "s|__PORT__|$port|; s|__CPORT__|$cport|" "$dir/docker-compose.yml"
  fi
  # apps read their secrets from .env
  sed -i 's|^    # env_file: .env|    env_file: .env|' "$dir/docker-compose.yml"
  chown deploy:deploy "$dir/docker-compose.yml"
  echo "wrote $dir/docker-compose.yml"
fi

if [ ! -f "$dir/app.conf" ]; then
  { echo "profile=$profile"; [ -n "$port" ] && echo "port=$port"; \
    echo "health_path=$hpath"; echo "image=$image"; } > "$dir/app.conf"
  chown deploy:deploy "$dir/app.conf"
  echo "wrote $dir/app.conf"
fi

if [ ! -f "$dir/.env" ]; then
  install -m 600 -o deploy -g deploy /dev/null "$dir/.env"
  echo "created $dir/.env (600)"
fi

if ! grep -qE "^$app( |\$)" "$APPS_LIST"; then
  printf '%s %s\n' "$app" "$dir" >> "$APPS_LIST"
  echo "apps.list: added '$app $dir'"
fi

# publish the HTTPS route (Caddy + Let's Encrypt); no-op if already present
if [ -x "$HUB_DIR/bin/caddy-sync.sh" ]; then
  "$HUB_DIR/bin/caddy-sync.sh" || echo "caddy-sync: skipped"
fi

echo "wb4-provision: ok ($app -> $dir)"
