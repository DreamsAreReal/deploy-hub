#!/bin/bash
# deploy-hub caddy-sync — regenerate /etc/caddy/Caddyfile from the app registry.
#
# Reads apps.list (one `<app> [dir]` per line) and each app's app.conf `port=`,
# and emits one HTTPS vhost per app:
#
#     <app>.<HOST_SLUG>.sslip.io {
#         reverse_proxy 127.0.0.1:<port>
#     }
#
# Caddy obtains and renews a real Let's Encrypt certificate for each name
# automatically (sslip.io resolves <slug> back to this IP). Plain :80 is served
# by Caddy too and redirects to https by default.
#
# Idempotent: it writes the file only if the content actually changed, and only
# reloads Caddy when it did. Safe to call from onboard.sh after adding an app.
set -euo pipefail

HUB_DIR="${HUB_DIR:-/opt/deploy-hub}"
APPS_LIST="$HUB_DIR/apps.list"
CADDYFILE="${CADDYFILE:-/etc/caddy/Caddyfile}"
# host as a DNS label: sslip.io maps 192-3-94-42.sslip.io -> 192.3.94.42
HOST_SLUG="${HOST_SLUG:-192-3-94-42}"
ACME_EMAIL="${ACME_EMAIL:-admin@${HOST_SLUG}.sslip.io}"

app_dir() { awk -v a="$1" '$1==a {print ($2 != "" ? $2 : "/opt/" a); exit}' "$APPS_LIST"; }
conf_get() { awk -F= -v k="$2" '$1==k {print substr($0, index($0,"=")+1); exit}' "$1/app.conf"; }

render() {
  printf '# Managed by deploy-hub caddy-sync.sh — do not edit by hand.\n'
  printf '{\n\temail %s\n}\n\n' "$ACME_EMAIL"
  while read -r app _rest; do
    [ -n "$app" ] || continue
    case "$app" in \#*) continue ;; esac
    local dir port
    dir=$(app_dir "$app")
    [ -n "$dir" ] && [ -f "$dir/app.conf" ] || continue
    # blue-green temporary route override: mid-cutover a deploy writes the standby
    # port to <dir>/.caddy-port so the route points at the standby WITHOUT mutating
    # app.conf (which stays on the permanent port). Absent/blank => permanent port.
    # Self-healing: the override is only honored while something actually listens
    # on it — after a reboot/OOM the standby (--restart no) is gone, so the stale
    # override is ignored and the route falls back to the permanent port.
    port=""
    if [ -f "$dir/.caddy-port" ]; then
      local ovr; ovr=$(tr -cd '0-9' < "$dir/.caddy-port" | head -c5)
      if [ -n "$ovr" ] && ss -tlnH "sport = :$ovr" 2>/dev/null | grep -q .; then
        port=$ovr
      fi
    fi
    [ -n "$port" ] || port=$(conf_get "$dir" port)
    printf '%s' "$port" | grep -Eq '^[0-9]{2,5}$' || continue
    printf '%s.%s.sslip.io {\n\treverse_proxy 127.0.0.1:%s\n}\n\n' \
      "$app" "$HOST_SLUG" "$port"
  done < "$APPS_LIST"
}

main() {
  local tmp; tmp=$(mktemp)
  render > "$tmp"

  # validate before touching the live file: a bad Caddyfile must not be applied
  if ! caddy validate --config "$tmp" --adapter caddyfile >/dev/null 2>&1; then
    echo "caddy-sync: generated Caddyfile failed validation, aborting" >&2
    rm -f "$tmp"; exit 1
  fi

  if [ -f "$CADDYFILE" ] && cmp -s "$tmp" "$CADDYFILE"; then
    echo "caddy-sync: Caddyfile already up to date (no reload)"
    rm -f "$tmp"; return 0
  fi

  install -m 644 "$tmp" "$CADDYFILE"
  rm -f "$tmp"
  systemctl reload caddy
  echo "caddy-sync: Caddyfile updated and Caddy reloaded"
}

main "$@"
