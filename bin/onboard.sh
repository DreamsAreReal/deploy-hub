#!/bin/bash
# onboard.sh — connect a GitHub repo to deploy-hub in one command.
#
#   ./onboard.sh <repo> --profile <static|bot|service> [options]
#
# What it does (prints the full plan first; every step is idempotent, so a
# second run on an onboarded repo reports "no changes"):
#   1. checks the repo fits the app model (Dockerfile, single built container)
#   2. commits the ~8-line caller stub to the DEFAULT branch (PR fallback when
#      the branch is protected)
#   3. sets repo secrets: VPS_SSH_KEY (+ TG_TOKEN/TG_CHAT_ID when .env-hub exists)
#   4. prepares /opt/<app>/ on the VPS over root SSH: docker-compose.yml from
#      the template, app.conf, empty .env (600), allowlist line in apps.list
#   5. prints what is left for you: paste the app secrets into /opt/<app>/.env
#      (variable names are extracted from the repo when possible)
#
# Options:
#   --profile static|bot|service   required
#   --port N          host port (127.0.0.1) for static/service; default: first
#                     free port >= 9001 (checked against app.confs and ss -tlnp)
#   --cport N         container port (default: 80 static, 8080 service)
#   --mem LIMIT       container mem_limit (default: 64m static, 128m bot, 256m service)
#   --health-path P   health gate path (default /)
#   --app NAME        app name (default: repo name, lowercased)
#   --dry-run         print the plan, change nothing
#
# Environment: VPS_HOST_ALIAS (root ssh alias, default `vpn`), DEPLOY_KEY
# (private deploy key, default ~/.ssh/deploy_hub_key). Optional bin/.env-hub
# with TG_TOKEN=/TG_CHAT_ID= (never committed; .gitignore covers it).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VPS_HOST_ALIAS="${VPS_HOST_ALIAS:-vpn}"
VPS_ADDR="${VPS_ADDR:-192.3.94.42}"
DEPLOY_KEY="${DEPLOY_KEY:-$HOME/.ssh/deploy_hub_key}"
HUB_ENV="$SCRIPT_DIR/.env-hub"
COMPOSE_TEMPLATE="$SCRIPT_DIR/../server/compose-template.yml"
HUB_REPO_SLUG="DreamsAreReal/deploy-hub"
APPS_LIST_REMOTE=/opt/deploy-hub/apps.list

# --- user-facing strings (en) -------------------------------------------------
S_USAGE="usage: ./onboard.sh <repo> --profile <static|bot|service> [--port N] [--cport N] [--mem LIMIT] [--health-path P] [--app NAME] [--dry-run]
       ./onboard.sh status | history <app>     # read-only, over the deploy channel"
S_BAD_PROFILE="error: --profile must be static, bot or service"
S_NO_REPO="error: repo not found on GitHub"
S_NO_DOCKERFILE="error: the repo has no Dockerfile — the app model is one built container per repo (add a Dockerfile first)"
S_MULTI_BUILD="error: the repo compose builds more than one service — multi-app repos are outside the app model (see README, 'Connecting a repo')"
S_NO_KEY="error: deploy key not found"
S_PORT_TAKEN="error: port already in use on the VPS"
S_NO_CHANGES="no changes — repo is already onboarded"
S_DRY="dry-run: nothing was changed"
S_LEFT_SECRETS="what's left for you: paste the app secrets into"
S_LEFT_NONE="nothing left to do by hand: push to the default branch and watch the deploy"
S_PROTECTED="default branch is protected: pushed the stub to a branch and opened a PR — merge it, then push"

say()  { printf '%s\n' "$*"; }
plan() { PLAN_LINES+=("$1"); say "  $1"; }
die()  { say "$1" >&2; exit 1; }

# --- convenience verbs: proxy read-only queries over the deploy channel ---------
case "${1:-}" in
  status|history)
    exec ssh -i "$DEPLOY_KEY" -o IdentitiesOnly=yes "deploy@$VPS_ADDR" "$*" < /dev/null
    ;;
esac

# --- parse args -----------------------------------------------------------------
REPO_ARG="" PROFILE="" PORT="" CPORT="" MEM="" HEALTH_PATH="/" APP="" DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --profile)     PROFILE="${2:?}"; shift 2 ;;
    --port)        PORT="${2:?}"; shift 2 ;;
    --cport)       CPORT="${2:?}"; shift 2 ;;
    --mem)         MEM="${2:?}"; shift 2 ;;
    --health-path) HEALTH_PATH="${2:?}"; shift 2 ;;
    --app)         APP="${2:?}"; shift 2 ;;
    --dry-run)     DRY=1; shift ;;
    -h|--help)     say "$S_USAGE"; exit 0 ;;
    -*)            die "$S_USAGE" ;;
    *)             REPO_ARG="$1"; shift ;;
  esac
done
[ -n "$REPO_ARG" ] || die "$S_USAGE"
case "$PROFILE" in static|bot|service) ;; *) die "$S_BAD_PROFILE" ;; esac

case "$REPO_ARG" in
  */*) REPO_SLUG="$REPO_ARG" ;;
  *)   REPO_SLUG="DreamsAreReal/$REPO_ARG" ;;
esac
REPO_NAME="${REPO_SLUG##*/}"
APP="${APP:-$(printf '%s' "$REPO_NAME" | tr '[:upper:]' '[:lower:]')}"
printf '%s' "$APP" | grep -Eq '^[a-z0-9][a-z0-9._-]{0,40}$' || die "error: bad app name: $APP"

# profile defaults
case "$PROFILE" in
  static)  CPORT="${CPORT:-80}";   MEM="${MEM:-64m}" ;;
  service) CPORT="${CPORT:-8080}"; MEM="${MEM:-256m}" ;;
  bot)     CPORT=""; PORT=""; MEM="${MEM:-128m}" ;;
esac

# --- gather facts (read-only) ----------------------------------------------------
say "onboard: $REPO_SLUG -> app '$APP' (profile $PROFILE)"
say "gathering facts..."

BRANCH=$(gh repo view "$REPO_SLUG" --json defaultBranchRef -q .defaultBranchRef.name 2>/dev/null) \
  || die "$S_NO_REPO: $REPO_SLUG"
OWNER_LC=$(printf '%s' "${REPO_SLUG%%/*}" | tr '[:upper:]' '[:lower:]')
IMAGE="ghcr.io/${OWNER_LC}/${APP}"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
git clone -q --depth 1 -b "$BRANCH" "https://github.com/${REPO_SLUG}.git" "$WORK/repo"

[ -f "$WORK/repo/Dockerfile" ] || die "$S_NO_DOCKERFILE"
# app model guard: a repo compose must not build more than one service
if compgen -G "$WORK/repo/docker-compose.y*ml" > /dev/null; then
  builds=$(grep -cE '^\s+build:' "$WORK"/repo/docker-compose.y*ml || true)
  [ "${builds:-0}" -le 1 ] || die "$S_MULTI_BUILD"
fi

# server-side facts over root ssh (single round-trip)
SERVER_FACTS=$(ssh "$VPS_HOST_ALIAS" bash -s "$APP" <<'RFACTS'
APP=$1
LIST=/opt/deploy-hub/apps.list
DIR=$(awk -v a="$APP" '$1==a {print ($2 != "" ? $2 : "/opt/" a); exit}' "$LIST" 2>/dev/null)
DIR=${DIR:-/opt/$APP}
echo "dir=$DIR"
echo "in_list=$(grep -cE "^$APP( |$)" "$LIST" 2>/dev/null || true)"
echo "dir_exists=$([ -d "$DIR" ] && echo 1 || echo 0)"
echo "conf_exists=$([ -f "$DIR/app.conf" ] && echo 1 || echo 0)"
echo "compose_exists=$([ -f "$DIR/docker-compose.yml" ] && echo 1 || echo 0)"
echo "env_exists=$([ -f "$DIR/.env" ] && echo 1 || echo 0)"
# ports taken: every app.conf port + everything listening
{ awk -F= '$1=="port" && $2!="" {print $2}' /opt/*/app.conf 2>/dev/null;
  ss -tlnp 2>/dev/null | awk 'NR>1 {n=split($4,a,":"); print a[n]}'; } | sort -un | tr '\n' ' ' | sed 's/^/ports_taken=/;s/ $//'
echo
RFACTS
)
facts() { printf '%s\n' "$SERVER_FACTS" | awk -F= -v k="$1" '$1==k {print substr($0, index($0,"=")+1); exit}'; }
DIR=$(facts dir)
PORTS_TAKEN=" $(facts ports_taken) "

# port: keep the one from an existing app.conf, else pick/validate
EXISTING_PORT=""
if [ "$(facts conf_exists)" = 1 ]; then
  EXISTING_PORT=$(ssh "$VPS_HOST_ALIAS" "awk -F= '\$1==\"port\"{print \$2}' '$DIR/app.conf'")
fi
if [ "$PROFILE" != bot ]; then
  if [ -n "$EXISTING_PORT" ]; then
    PORT="$EXISTING_PORT"
  elif [ -z "$PORT" ]; then
    p=9001; while printf '%s' "$PORTS_TAKEN" | grep -q " $p "; do p=$((p+1)); done; PORT=$p
  elif printf '%s' "$PORTS_TAKEN" | grep -q " $PORT "; then
    die "$S_PORT_TAKEN: $PORT (taken:$PORTS_TAKEN)"
  fi
fi

# stub: compare with what the repo already has
STUB_PATH="$WORK/repo/.github/workflows/deploy.yml"
STUB_WANT=$(cat <<EOF
on:
  push: {branches: [$BRANCH]}
permissions: {contents: read, packages: write}
jobs:
  deploy:
    uses: $HUB_REPO_SLUG/.github/workflows/deploy.yml@main
    with: {app: $APP}
    secrets: inherit
EOF
)
STUB_STATE=missing
if [ -f "$STUB_PATH" ]; then
  if [ "$(cat "$STUB_PATH")" = "$STUB_WANT" ]; then STUB_STATE=ok; else STUB_STATE=differs; fi
fi

SECRETS_HAVE=$(gh secret list -R "$REPO_SLUG" --json name -q '.[].name' 2>/dev/null | tr '\n' ' ')
have_secret() { printf '%s' " $SECRETS_HAVE " | grep -q " $1 "; }

# app .env variable names, best effort: .env.example in the repo, else compose vars
ENV_VARS=""
for f in .env.example .env.sample env.example; do
  if [ -f "$WORK/repo/$f" ]; then
    ENV_VARS=$(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$WORK/repo/$f" | tr -d = | tr '\n' ' ' || true)
    break
  fi
done
if [ -z "$ENV_VARS" ] && compgen -G "$WORK/repo/docker-compose.y*ml" > /dev/null; then
  ENV_VARS=$(grep -ohE '\$\{[A-Za-z_][A-Za-z0-9_]*[:}?-]' "$WORK"/repo/docker-compose.y*ml \
    | sed -E 's/^\$\{//; s/[:}?-]$//' | sort -u | tr '\n' ' ' || true)
fi

# --- plan -------------------------------------------------------------------------
PLAN_LINES=()
say "plan:"
[ "$STUB_STATE" = ok ]        || plan "commit caller stub to $REPO_SLUG@$BRANCH ($STUB_STATE)"
have_secret VPS_SSH_KEY       || plan "set secret VPS_SSH_KEY on $REPO_SLUG"
if [ -f "$HUB_ENV" ]; then
  have_secret TG_TOKEN        || plan "set secret TG_TOKEN on $REPO_SLUG (from .env-hub)"
  have_secret TG_CHAT_ID      || plan "set secret TG_CHAT_ID on $REPO_SLUG (from .env-hub)"
else
  say "  note: no bin/.env-hub — skipping TG_TOKEN/TG_CHAT_ID (CI failure cards stay off)"
fi
[ "$(facts dir_exists)" = 1 ]     || plan "create $DIR on the VPS"
[ "$(facts compose_exists)" = 1 ] || plan "write $DIR/docker-compose.yml (image $IMAGE, ${PORT:+127.0.0.1:$PORT->$CPORT, }mem $MEM)"
[ "$(facts conf_exists)" = 1 ]    || plan "write $DIR/app.conf (profile=$PROFILE${PORT:+ port=$PORT} health_path=$HEALTH_PATH)"
[ "$(facts env_exists)" = 1 ]     || plan "create empty $DIR/.env (600, owner deploy)"
[ "$(facts in_list)" != 0 ]       || plan "allowlist: add '$APP $DIR' to $APPS_LIST_REMOTE"

if [ ${#PLAN_LINES[@]} -eq 0 ]; then
  say "$S_NO_CHANGES"
  exit 0
fi
if [ "$DRY" = 1 ]; then
  say "$S_DRY"
  exit 0
fi

# --- apply ------------------------------------------------------------------------
say "applying..."

if ! have_secret VPS_SSH_KEY; then
  [ -f "$DEPLOY_KEY" ] || die "$S_NO_KEY: $DEPLOY_KEY"
  gh secret set VPS_SSH_KEY -R "$REPO_SLUG" < "$DEPLOY_KEY"
  say "  secret VPS_SSH_KEY: set"
fi
if [ -f "$HUB_ENV" ]; then
  for s in TG_TOKEN TG_CHAT_ID; do
    if ! have_secret "$s"; then
      v=$(awk -F= -v k="$s" '$1==k {print substr($0, index($0,"=")+1); exit}' "$HUB_ENV")
      [ -n "$v" ] && { printf '%s' "$v" | gh secret set "$s" -R "$REPO_SLUG"; say "  secret $s: set"; }
    fi
  done
fi

# server prep (idempotent; root ssh)
BOT_PROFILE=$([ "$PROFILE" = bot ] && echo 1 || echo 0)
# shellcheck disable=SC2087  # intentional: $(cat template) expands locally,
# remote-side variables are escaped as \$ throughout the heredoc
ssh "$VPS_HOST_ALIAS" bash -s "$APP" "$DIR" "$PROFILE" "${PORT:-}" "${CPORT:-}" "$MEM" "$HEALTH_PATH" "$IMAGE" "$BOT_PROFILE" <<REMOTE
set -euo pipefail
APP=\$1 DIR=\$2 PROFILE=\$3 PORT=\$4 CPORT=\$5 MEM=\$6 HPATH=\$7 IMAGE=\$8 BOT=\$9
install -d -m 755 -o deploy -g deploy "\$DIR"
if [ ! -f "\$DIR/docker-compose.yml" ]; then
  cat > "\$DIR/docker-compose.yml" <<'TPL'
$(cat "$COMPOSE_TEMPLATE")
TPL
  sed -i "s|__NAME__|\$APP|; s|__MEM_LIMIT__|\$MEM|" "\$DIR/docker-compose.yml"
  if [ "\$BOT" = 1 ]; then
    sed -i '/^    ports:/,+1d' "\$DIR/docker-compose.yml"
  else
    sed -i "s|__PORT__|\$PORT|; s|__CPORT__|\$CPORT|" "\$DIR/docker-compose.yml"
  fi
  # apps with secrets read them from .env
  sed -i 's|^    # env_file: .env|    env_file: .env|' "\$DIR/docker-compose.yml"
  chown deploy:deploy "\$DIR/docker-compose.yml"
  echo "  \$DIR/docker-compose.yml: written"
fi
if [ ! -f "\$DIR/app.conf" ]; then
  { echo "profile=\$PROFILE"; [ -n "\$PORT" ] && echo "port=\$PORT"; echo "health_path=\$HPATH"; echo "image=\$IMAGE"; } > "\$DIR/app.conf"
  chown deploy:deploy "\$DIR/app.conf"
  echo "  \$DIR/app.conf: written"
fi
if [ ! -f "\$DIR/.env" ]; then
  install -m 600 -o deploy -g deploy /dev/null "\$DIR/.env"
  echo "  \$DIR/.env: created (600, empty)"
fi
if ! grep -qE "^\$APP( |\$)" $APPS_LIST_REMOTE; then
  printf '%s %s\n' "\$APP" "\$DIR" >> $APPS_LIST_REMOTE
  echo "  apps.list: added '\$APP \$DIR'"
fi
REMOTE

# caller stub commit (last: the push triggers the first deploy)
if [ "$STUB_STATE" != ok ]; then
  mkdir -p "$WORK/repo/.github/workflows"
  printf '%s\n' "$STUB_WANT" > "$STUB_PATH"
  git -C "$WORK/repo" add .github/workflows/deploy.yml
  git -C "$WORK/repo" -c user.name="deploy-hub onboard" -c user.email="onboard@deploy-hub.local" \
    commit -q -m "ci: deploy to VPS via deploy-hub"
  if git -C "$WORK/repo" push -q origin "$BRANCH" 2>/dev/null; then
    say "  stub: committed to $BRANCH"
  else
    PRB="deploy-hub-onboard"
    git -C "$WORK/repo" checkout -q -b "$PRB"
    git -C "$WORK/repo" push -q -f origin "$PRB"
    gh pr create -R "$REPO_SLUG" --head "$PRB" --base "$BRANCH" \
      --title "ci: deploy to VPS via deploy-hub" \
      --body "Caller stub generated by onboard.sh." >/dev/null
    say "  $S_PROTECTED"
  fi
fi

say "done."
if [ "$PROFILE" = static ] && [ -z "$ENV_VARS" ]; then
  say "$S_LEFT_NONE"
else
  say "$S_LEFT_SECRETS $DIR/.env on the VPS"
  if [ -n "$ENV_VARS" ]; then
    say "  variables found in the repo: $ENV_VARS"
  else
    say "  (no .env.example/compose vars found — check the app README for required variables)"
  fi
fi
