#!/usr/bin/env python3
"""deploy-hub Telegram control bot (long-polling, stdlib only).

A button-driven control surface over the existing runner.sh on this VPS. The
main entry is an inline-keyboard menu (Apps / Status / Help); navigation edits
the same message instead of spamming new ones. Slash commands (/status /apps
/logs /rollback /redeploy /history /help) stay as a fallback.

The bot exposes only a FIXED set of actions and never runs arbitrary shell.
All privileged work is delegated to runner.sh, which owns the app allowlist and
the deploy logic — the bot adds no new way around that allowlist.

SECURITY (P0): every update AND every callback_query is gated on
chat.id == AUTHORIZED_CHAT_ID and from.id == AUTHORIZED_CHAT_ID. Any other
chat/user is ignored and the attempt (chat_id, username) is journaled. There is
no code path that acts on an unauthorized chat. Destructive actions
(rollback / redeploy) require an explicit inline confirmation.

Runs as a systemd service under user `deploy` (in the docker group, able to
read the cloudflared tunnel log). No inbound port: it long-polls getUpdates.
The token is read from telegram.env (600, owner deploy) and is never placed in
argv, git, or logs.

Human-facing UI text is Russian; identifiers, logs and code stay English.
"""
import base64
import ctypes
import html
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

# --- configuration ------------------------------------------------------------
HUB_DIR = os.environ.get("HUB_DIR", "/opt/deploy-hub")
TELEGRAM_ENV = os.path.join(HUB_DIR, "telegram.env")
GITHUB_ENV = os.path.join(HUB_DIR, "github.env")   # GH_TOKEN=, GH_OWNER= (600, deploy)
VPS_SSH_KEY = os.path.join(HUB_DIR, "vps_ssh_key")  # private deploy key (600) for the caller secret
PROVISION = os.path.join(HUB_DIR, "bin", "wb4-provision.sh")  # root helper via sudoers
RUNNER = os.path.join(HUB_DIR, "bin", "runner.sh")
APPS_LIST = os.path.join(HUB_DIR, "apps.list")
LOG_FILE = os.path.join(HUB_DIR, "deploys.log")
BOT_LOG = os.path.join(HUB_DIR, "bot.log")
HUB_REPO_SLUG = "DreamsAreReal/deploy-hub"
# per-profile compose defaults (mirror onboard.sh)
PROFILE_DEFAULTS = {
    "static":  {"cport": "80",   "mem": "64m"},
    "service": {"cport": "8080", "mem": "256m"},
    "bot":     {"cport": "",     "mem": "128m"},
}
# every app is published by Caddy at a stable HTTPS URL (auto Let's Encrypt on
# sslip.io): https://<app>.<HOST_SLUG>.sslip.io (WB2). This replaces the old
# nginx :80 paths and the rotating cloudflared tunnel — the URL no longer moves.
HOST_SLUG = "192-3-94-42"

APP_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,40}$")
TELEGRAM_TEXT_LIMIT = 4096

# --- background monitor thresholds (defaults; overridable via alerts.conf) -----
ALERTS_CONF = os.path.join(HUB_DIR, "alerts.conf")
DEFAULT_ALERTS = {
    "interval_sec": 60,      # how often the monitor samples
    "disk_pct": 85,          # disk use% above this -> alert
    "ram_min_mib": 80,       # available RAM below this (MiB) -> alert
    "cooldown_sec": 10800,   # min seconds between repeats of the same resource alert (3h)
}

# consistent status glyphs (RS-05: one glyph tells the state at a glance)
GLYPH_HEALTHY = "\U0001f680"  # 🚀 running / healthy / passing gate
GLYPH_RUNNING = "⚠️"          # ⚠️ running, no health signal (warning)
GLYPH_DOWN = "\U0001f534"     # 🔴 missing / unhealthy / down

# --- human-facing UI strings (ru) ---------------------------------------------
T_MENU = "<b>deploy-hub</b>\nПанель управления деплоями."
T_HELP = (
    "❔ <b>Справка</b>\n\n"
    "<b>Команды</b>\n"
    "/menu — главное меню\n"
    "/status — статус всех приложений\n"
    "/server — ресурсы сервера (RAM, диск, uptime)\n"
    "/apps — список приложений\n"
    "/logs &lt;app&gt; — хвост логов контейнера\n"
    "/history &lt;app&gt; — журнал деплоев\n"
    "/rollback &lt;app&gt; — откат на прошлый sha\n"
    "/redeploy &lt;app&gt; — передеплой текущего sha\n\n"
    "<b>Действия в карточке</b>\n"
    "\U0001f4dc Логи · ⏪ Откат · \U0001f504 Редеплой · ♻️ Обновить\n\n"
    "Откат и редеплой требуют подтверждения."
)
T_MENU_HINT = "Выберите раздел:"
T_APPS_HINT = "Выберите приложение:"
T_NO_APPS = "Приложений пока нет."
# breadcrumbs shown in section titles (RS-05: user never loses context)
CRUMB_APPS = "Приложения"
CRUMB_STATUS = "Статус"
# app card labels (RS-05 checklist): the card body is built in screen_app() with
# a status glyph + breadcrumb title, <code> for copyables, a clickable URL,
# bullet metrics, blank-line separators and a final one-line verdict
T_CARD_URL_LABEL = "Открыть приложение"
T_CARD_NO_URL = "URL нет (профиль bot)"
V_HEALTHY = "\U0001f680 Здоров — все системы в норме"
V_WARNING = "⚠️ Запущен, health-сигнала нет"
V_DOWN = "\U0001f534 Недоступен — контейнер не отвечает"
T_UNKNOWN_APP = "Неизвестное приложение: <code>{}</code>"
T_NEED_APP = "Использование: {} &lt;app&gt;"
T_UNKNOWN_CMD = "Неизвестная команда. Откройте /menu."
T_CONFIRM_ROLLBACK = "⚠️ Откатить <b>{}</b> на предыдущий sha?\n\nЭто изменит прод и запустит развёртывание."
T_CONFIRM_REDEPLOY = "\U0001f504 Передеплоить <b>{}</b> на текущий sha?\n\nКонтейнер будет пересоздан."
T_WORKING = "⏳ Выполняю…"
T_LOADING = "⏳ Выполняю операцию, подождите…"
T_DONE_ROLLBACK = "Откат выполнен"
T_DONE_REDEPLOY = "Редеплой выполнен"
T_FAILED = "Ошибка (см. карточку)"
T_REFRESHED = "Обновлено"
T_RUNNER_FAIL = "runner error (exit {}):\n<pre>{}</pre>"
T_SERVER_TITLE = "\U0001f5a5 <b>Сервер</b>"
T_SERVER_BODY = (
    "{title}\n\n"
    "\U0001f4ca <b>Ресурсы</b>\n"
    "  • RAM: {ram_used}/{ram_total} MiB (свободно {ram_avail})\n"
    "  • диск /: {disk_pct}% занято (свободно {disk_free})\n"
    "  • uptime: {uptime}\n"
    "  • контейнеров: {containers}"
)
# resource line reused in /status and the app card: RAM used/total + available, disk
T_RES_LINE = "\U0001f5a5 RAM {ram_used}/{ram_total} MiB (avail {ram_avail}) • диск {disk_pct}% (свободно {disk_free})"
# background-monitor alert texts (owner only)
A_UNHEALTHY = "⚠️ <b>{}</b> unhealthy ({})"
A_RECOVERED = "✅ <b>{}</b> recovered"
A_DISK = "\U0001f534 диск {}% (свободно {})"
A_DISK_OK = "✅ диск в норме ({}%)"
A_RAM = "\U0001f534 RAM: свободно всего {} MiB"
A_RAM_OK = "✅ RAM в норме (свободно {} MiB)"

# WB4 "Connect a repo" flow
B_CONNECT = "➕ Подключить"
B_ST_STATIC = "\U0001f310 static"
B_ST_SERVICE = "⚙️ service"
B_ST_BOT = "\U0001f916 bot"
B_DO_CONNECT = "✅ Подключить"
B_CHANGE_TYPE = "\U0001f527 Сменить тип"
B_MORE = "ещё »"
C_TITLE = "<b>Подключить репозиторий</b>\nВыберите репозиторий владельца:"
C_NONE = "Нет подходящих репозиториев (все подключены или недоступны)."
C_GH_FAIL = "Не удалось получить список репозиториев (GitHub API)."
C_ALREADY = "Репозиторий <b>{}</b> уже подключён."
C_PICK_PROFILE = "<b>{}</b>\nВыберите профиль приложения:"
C_CONFIRM = "Подключить <b>{}</b> как <b>{}</b>?\nБудет создан workflow и секреты в репозитории, каталог на сервере и HTTPS-маршрут."
C_WORKING = "Подключаю <b>{}</b>… это займёт до минуты."
C_STEP = "• {}"
C_OK = "✅ <b>{}</b> подключён\n\U0001f517 https://{}.{}.sslip.io\nПервый деплой запущен (push в ветку {})."
C_FAIL = "❌ Не удалось подключить <b>{}</b>: {}"
C_NO_DOCKERFILE = "в репозитории нет Dockerfile (нужен один собираемый контейнер)"
# auto-detection result screen + reasons (owner-facing)
C_DETECT_SERVICE = "\U0001f50e <b>{}</b>\nОпределил: <b>service</b> ({}) — дам HTTPS-адрес."
C_DETECT_BOT = "\U0001f50e <b>{}</b>\nОпределил: <b>bot</b> ({}) — без URL, health по процессу."
D_EXPOSE = "EXPOSE {} в Dockerfile"
D_COMPOSE_PORT = "порт {} в compose"
D_COMPOSE_PORTS = "порт в compose"
D_BOT = "нет EXPOSE и портов"
# onboarding step lines + error reasons (owner-facing)
C_STEP_SERVER = "создаю каталог и маршрут на сервере"
C_STEP_SECRETS = "выставляю секреты репозитория"
C_STEP_WORKFLOW = "коммичу workflow (запускает первый деплой)"
E_NO_GH = "github.env недоступен"
E_NO_REPO = "репозиторий не найден"
E_PROVISION = "provision: {}"
E_NO_KEY = "vps_ssh_key недоступен"
E_SECRET = "не удалось выставить VPS_SSH_KEY"
E_WORKFLOW = "не удалось записать workflow"

# button labels (ru text, <=1 glyph each)
B_APPS = "\U0001f4e6 Приложения"
B_STATUS = "\U0001f4ca Статус"
B_SERVER = "\U0001f5a5 Сервер"
B_HELP = "❔ Помощь"
B_BACK = "« Назад"
B_LOGS = "\U0001f4dc Логи"
B_ROLLBACK = "⏪ Откат"
B_REDEPLOY = "\U0001f504 Редеплой"
B_REFRESH = "♻️ Обновить"
B_YES = "✅ Да"
B_CANCEL = "✖️ Отмена"


# --- config loading -----------------------------------------------------------
def load_config():
    """Read TG_TOKEN / TG_CHAT_ID from telegram.env. The token never leaves
    this process (not logged, not passed to child argv)."""
    token = chat_id = None
    with open(TELEGRAM_ENV, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line.startswith("TG_TOKEN="):
                token = line[len("TG_TOKEN="):]
            elif line.startswith("TG_CHAT_ID="):
                chat_id = line[len("TG_CHAT_ID="):]
    if not token or not chat_id:
        sys.stderr.write("bot: TG_TOKEN/TG_CHAT_ID missing in telegram.env\n")
        sys.exit(1)
    return token, int(chat_id)


def load_alerts_conf():
    """Monitor thresholds: defaults, optionally overridden by alerts.conf
    (key=value lines). Unknown/invalid keys are ignored."""
    conf = dict(DEFAULT_ALERTS)
    try:
        with open(ALERTS_CONF, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, _, v = line.partition("=")
                k = k.strip()
                if k in conf:
                    try:
                        conf[k] = int(v.strip())
                    except ValueError:
                        pass
    except OSError:
        pass
    return conf


# --- journaling ---------------------------------------------------------------
def bot_log(msg):
    ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    try:
        with open(BOT_LOG, "a", encoding="utf-8") as fh:
            fh.write(f"[{ts}] {msg}\n")
    except OSError:
        pass  # logging must never crash the poll loop


def log_denied(chat_id, username):
    """P0 audit trail: record a rejected update's chat_id + username. Never
    echoes message text (could contain anything)."""
    bot_log(f"DENIED chat_id={chat_id} username={username}")


# --- Telegram API -------------------------------------------------------------
class Api:
    def __init__(self, token):
        self._base = f"https://api.telegram.org/bot{token}/"

    def _call(self, method, params, timeout):
        data = urllib.parse.urlencode(params).encode()
        req = urllib.request.Request(self._base + method, data=data)
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.load(resp)

    def get_updates(self, offset, timeout=50):
        try:
            r = self._call("getUpdates",
                           {"offset": offset, "timeout": timeout,
                            "allowed_updates": json.dumps(["message", "callback_query"])},
                           timeout=timeout + 15)
            return r.get("result", [])
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as e:
            bot_log(f"getUpdates error: {type(e).__name__}")
            time.sleep(3)
            return []

    def send(self, chat_id, text, reply_markup=None):
        params = {"chat_id": chat_id, "text": text[:TELEGRAM_TEXT_LIMIT],
                  "parse_mode": "HTML", "disable_web_page_preview": "true"}
        if reply_markup is not None:
            params["reply_markup"] = json.dumps(reply_markup)
        try:
            return self._call("sendMessage", params, timeout=15)
        except (urllib.error.URLError, TimeoutError) as e:
            bot_log(f"sendMessage error: {type(e).__name__}")
            return None

    def edit(self, chat_id, message_id, text, reply_markup=None):
        """Edit an existing message in place (navigation without spam)."""
        params = {"chat_id": chat_id, "message_id": message_id,
                  "text": text[:TELEGRAM_TEXT_LIMIT], "parse_mode": "HTML",
                  "disable_web_page_preview": "true"}
        if reply_markup is not None:
            params["reply_markup"] = json.dumps(reply_markup)
        try:
            return self._call("editMessageText", params, timeout=15)
        except urllib.error.HTTPError as e:
            # "message is not modified" is a benign 400 when the content is
            # identical (e.g. Refresh with no change) — ignore it
            if e.code == 400:
                return None
            bot_log(f"editMessageText error: {e.code}")
            return None
        except (urllib.error.URLError, TimeoutError) as e:
            bot_log(f"editMessageText error: {type(e).__name__}")
            return None

    def answer_callback(self, cb_id, text=None):
        params = {"callback_query_id": cb_id}
        if text:
            params["text"] = text
        try:
            self._call("answerCallbackQuery", params, timeout=10)
        except (urllib.error.URLError, TimeoutError):
            pass

    def set_commands(self, commands):
        """setMyCommands: the "/" quick-command menu next to the input box."""
        try:
            self._call("setMyCommands",
                       {"commands": json.dumps(commands)}, timeout=10)
            return True
        except (urllib.error.URLError, TimeoutError) as e:
            bot_log(f"setMyCommands error: {type(e).__name__}")
            return False


# --- app registry (same source of truth as runner) ----------------------------
def list_apps():
    apps = []
    try:
        with open(APPS_LIST, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                apps.append(line.split()[0])
    except OSError:
        pass
    return apps


def app_dir(app):
    try:
        with open(APPS_LIST, encoding="utf-8") as fh:
            for line in fh:
                parts = line.split()
                if parts and parts[0] == app:
                    return parts[1] if len(parts) > 1 else f"/opt/{app}"
    except OSError:
        return None
    return None


def valid_app(app):
    return bool(app) and bool(APP_RE.match(app)) and app in list_apps()


# --- runner + host reads (fixed command set only) -----------------------------
def run_runner(args):
    """Invoke runner.sh in its local-CLI mode. args is a fixed list built from
    validated tokens — never a shell string, so no injection surface."""
    try:
        proc = subprocess.run([RUNNER, *args], capture_output=True, text=True,
                              timeout=180)
        out = (proc.stdout or "") + (proc.stderr or "")
        return proc.returncode, out.strip()
    except subprocess.TimeoutExpired:
        return 124, "runner timed out"
    except OSError as e:
        return 1, f"cannot exec runner: {e}"


# --- GitHub REST client (WB4) -------------------------------------------------
# The token is read from github.env and used only as a Bearer header — never in
# argv, never logged. Secret values are sealed with libsodium (crypto_box_seal)
# via ctypes against the system libsodium.so, so there is no pip dependency.
GH_API = "https://api.github.com"


def load_github_env():
    """Return (token, owner) from github.env, or (None, None) if unavailable."""
    token = owner = None
    try:
        with open(GITHUB_ENV, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line.startswith("GH_TOKEN="):
                    token = line[len("GH_TOKEN="):]
                elif line.startswith("GH_OWNER="):
                    owner = line[len("GH_OWNER="):]
    except OSError:
        return None, None
    return token, owner


def gh_api(token, method, path, body=None, raw_url=None):
    """One GitHub REST call. Returns (status, parsed_json_or_bytes). The token is
    passed only as an Authorization header."""
    url = raw_url or (GH_API + path)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    req.add_header("User-Agent", "deploy-hub-bot")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
            try:
                return resp.status, json.loads(payload) if payload else {}
            except json.JSONDecodeError:
                return resp.status, payload
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}")
        except (json.JSONDecodeError, OSError):
            return e.code, {}
    except (urllib.error.URLError, TimeoutError) as e:
        bot_log(f"gh_api error {method} {path}: {type(e).__name__}")
        return 0, {}


def gh_list_repos(token, owner):
    """Owner's repos: non-archived, non-fork, newest first. Returns a list of
    {name, default_branch}."""
    repos = []
    page = 1
    while page <= 5:  # up to 500 repos; plenty for one account
        status, data = gh_api(
            token, "GET",
            f"/user/repos?per_page=100&page={page}&sort=pushed&affiliation=owner")
        if status != 200 or not isinstance(data, list) or not data:
            break
        for r in data:
            if r.get("archived") or r.get("fork"):
                continue
            if (r.get("owner") or {}).get("login") != owner:
                continue
            repos.append({"name": r["name"],
                          "default_branch": r.get("default_branch", "main")})
        if len(data) < 100:
            break
        page += 1
    return repos


def gh_get_file(token, owner, repo, branch, path):
    """Return the decoded text of a repo file, or None if it is absent."""
    status, data = gh_api(
        token, "GET",
        f"/repos/{owner}/{repo}/contents/{path}?ref={branch}")
    if status != 200 or not isinstance(data, dict) or "content" not in data:
        return None
    try:
        return base64.b64decode(data["content"]).decode("utf-8", "replace")
    except (ValueError, KeyError):
        return None


def gh_repo_has_dockerfile(token, owner, repo, branch):
    return gh_get_file(token, owner, repo, branch, "Dockerfile") is not None


# --- profile auto-detection (WB4 one-tap onboarding) --------------------------
EXPOSE_RE = re.compile(r"^\s*EXPOSE\s+(\d{2,5})", re.MULTILINE | re.IGNORECASE)
# compose published port: "  - 8080:80" / "  - 127.0.0.1:3000:3000" / "ports:"
COMPOSE_PORT_RE = re.compile(r"^\s*-\s*[\"']?(?:\d{1,3}(?:\.\d{1,3}){3}:)?(\d{2,5}):\d{2,5}",
                             re.MULTILINE)
COMPOSE_HAS_PORTS_RE = re.compile(r"^\s*ports\s*:", re.MULTILINE)


def detect_profile(token, owner, repo, branch):
    """Infer the app profile from repo contents (read-only).

    Returns (profile, cport, reason) where profile is 'service' | 'bot', cport is
    the container port to expose for service apps (or "" for bot), and reason is a
    short human string. Returns (None, "", reason) when there is no Dockerfile —
    the caller must refuse rather than guess.

    Heuristic: EXPOSE in the Dockerfile or a published port in compose => a web
    app (service); a Dockerfile with neither => a bot (no URL). static is not
    auto-detected — it is only a manual override (cosmetic health-path split).
    """
    dockerfile = gh_get_file(token, owner, repo, branch, "Dockerfile")
    if dockerfile is None:
        return None, "", C_NO_DOCKERFILE

    m = EXPOSE_RE.search(dockerfile)
    if m:
        return "service", m.group(1), D_EXPOSE.format(m.group(1))

    # no EXPOSE: check a root compose for a published port
    for cf in ("docker-compose.yml", "docker-compose.yaml", "compose.yml"):
        compose = gh_get_file(token, owner, repo, branch, cf)
        if compose is None:
            continue
        pm = COMPOSE_PORT_RE.search(compose)
        if pm:
            return "service", pm.group(1), D_COMPOSE_PORT.format(pm.group(1))
        if COMPOSE_HAS_PORTS_RE.search(compose):
            return "service", "", D_COMPOSE_PORTS
        break

    # Dockerfile present, no exposed port anywhere => a bot
    return "bot", "", D_BOT


def gh_existing_workflow(token, owner, repo, branch):
    """Return the current deploy.yml (path content sha) if present, else None."""
    status, data = gh_api(
        token, "GET",
        f"/repos/{owner}/{repo}/contents/.github/workflows/deploy.yml?ref={branch}")
    if status == 200 and isinstance(data, dict):
        return data.get("sha")
    return None


def caller_stub(app, branch):
    return (
        "on:\n"
        f"  push: {{branches: [{branch}]}}\n"
        "permissions: {contents: read, packages: write}\n"
        "jobs:\n"
        "  deploy:\n"
        f"    uses: {HUB_REPO_SLUG}/.github/workflows/deploy.yml@main\n"
        f"    with: {{app: {app}}}\n"
        "    secrets: inherit\n")


def gh_put_workflow(token, owner, repo, branch, app, prev_sha):
    """Create/update .github/workflows/deploy.yml on the default branch."""
    content = caller_stub(app, branch)
    body = {
        "message": "ci: deploy to VPS via deploy-hub",
        "content": base64.b64encode(content.encode()).decode(),
        "branch": branch,
    }
    if prev_sha:
        body["sha"] = prev_sha
    status, _ = gh_api(
        token, "PUT",
        f"/repos/{owner}/{repo}/contents/.github/workflows/deploy.yml", body)
    return status in (200, 201)


def _seal(public_key_b64, secret_value):
    """libsodium crypto_box_seal via ctypes (sealed box, what the GitHub secrets
    API expects). No pip dependency: binds the system libsodium.so."""
    lib = None
    for name in ("libsodium.so.23", "libsodium.so", "libsodium.so.26"):
        try:
            lib = ctypes.CDLL(name)
            break
        except OSError:
            continue
    if lib is None:
        raise RuntimeError("libsodium not found")
    lib.sodium_init()
    pk = base64.b64decode(public_key_b64)
    msg = secret_value.encode()
    SEALBYTES = 48  # crypto_box_SEALBYTES
    out = ctypes.create_string_buffer(len(msg) + SEALBYTES)
    rc = lib.crypto_box_seal(out, msg, ctypes.c_ulonglong(len(msg)), pk)
    if rc != 0:
        raise RuntimeError("crypto_box_seal failed")
    return base64.b64encode(out.raw).decode()


def gh_set_secret(token, owner, repo, name, value):
    """Set a repo Actions secret (fetch public key, seal, PUT)."""
    status, key = gh_api(token, "GET",
                         f"/repos/{owner}/{repo}/actions/secrets/public-key")
    if status != 200 or not isinstance(key, dict):
        return False
    try:
        sealed = _seal(key["key"], value)
    except (RuntimeError, KeyError) as e:
        bot_log(f"seal error: {e}")
        return False
    status, _ = gh_api(
        token, "PUT",
        f"/repos/{owner}/{repo}/actions/secrets/{name}",
        {"encrypted_value": sealed, "key_id": key["key_id"]})
    return status in (201, 204)


# --- onboarding orchestration (WB4) -------------------------------------------
def next_free_port():
    """First free 127.0.0.1 port >= 9001, checked against every app.conf port
    and everything currently listening — same rule as onboard.sh."""
    taken = set()
    for app in list_apps():
        d = app_dir(app)
        conf = os.path.join(d, "app.conf") if d else None
        if conf and os.path.isfile(conf):
            try:
                with open(conf, encoding="utf-8") as fh:
                    for line in fh:
                        if line.startswith("port="):
                            taken.add(line.strip()[len("port="):])
            except OSError:
                pass
    try:
        out = subprocess.run(["ss", "-tlnH"], capture_output=True, text=True,
                             timeout=10).stdout
        for line in out.splitlines():
            fields = line.split()
            if len(fields) >= 4:
                taken.add(fields[3].rsplit(":", 1)[-1])
    except (subprocess.TimeoutExpired, OSError):
        pass
    p = 9001
    while str(p) in taken:
        p += 1
    return str(p)


def provision_server(app, profile, port, cport, mem, health_path, image):
    """Run the privileged server-side step (create /opt/<app>, apps.list line,
    Caddy route) via the narrow sudoers helper. Fixed argument list, no shell."""
    try:
        proc = subprocess.run(
            ["sudo", "-n", PROVISION, app, profile, port or "", cport or "",
             mem, health_path, image],
            capture_output=True, text=True, timeout=120)
        return proc.returncode == 0, (proc.stdout + proc.stderr).strip()
    except (subprocess.TimeoutExpired, OSError) as e:
        return False, f"provision failed: {e}"


def wb4_onboard(app, repo, profile, progress=None):
    """End-to-end onboarding of one repo. Returns (ok, branch_or_errmsg).

    Steps: validate repo -> GitHub workflow + secrets -> server provisioning.
    The first deploy is triggered by the workflow commit itself. `progress` is
    an optional callback(str) for streaming step lines to the chat.
    """
    def step(msg):
        if progress:
            progress(msg)

    token, owner = load_github_env()
    if not token or not owner:
        return False, E_NO_GH

    # repo facts
    status, meta = gh_api(token, "GET", f"/repos/{owner}/{repo}")
    if status != 200 or not isinstance(meta, dict):
        return False, E_NO_REPO
    branch = meta.get("default_branch", "main")
    if not gh_repo_has_dockerfile(token, owner, repo, branch):
        return False, C_NO_DOCKERFILE

    defaults = PROFILE_DEFAULTS.get(profile, PROFILE_DEFAULTS["static"])
    cport = defaults["cport"]
    mem = defaults["mem"]
    # service: prefer the container port detected from EXPOSE/compose over the
    # generic default, so the reverse-proxy targets the port the app listens on
    if profile == "service":
        _p, detected_cport, _r = detect_profile(token, owner, repo, branch)
        if detected_cport:
            cport = detected_cport
    port = "" if profile == "bot" else next_free_port()
    image = f"ghcr.io/{owner.lower()}/{app}"

    # server side FIRST: if we cannot create /opt/<app>, do not touch the repo
    step(C_STEP_SERVER)
    ok, out = provision_server(app, profile, port, cport, mem, "/", image)
    if not ok:
        return False, E_PROVISION.format(out[-200:])

    # repo secrets: VPS_SSH_KEY (from the local key) + TG creds
    step(C_STEP_SECRETS)
    try:
        with open(VPS_SSH_KEY, encoding="utf-8") as fh:
            key_material = fh.read()
    except OSError:
        return False, E_NO_KEY
    if not gh_set_secret(token, owner, repo, "VPS_SSH_KEY", key_material):
        return False, E_SECRET
    tg_token, tg_chat = load_config()
    gh_set_secret(token, owner, repo, "TG_TOKEN", tg_token)
    gh_set_secret(token, owner, repo, "TG_CHAT_ID", str(tg_chat))

    # workflow last: its commit triggers the first deploy
    step(C_STEP_WORKFLOW)
    prev = gh_existing_workflow(token, owner, repo, branch)
    if not gh_put_workflow(token, owner, repo, branch, app, prev):
        return False, E_WORKFLOW

    return True, branch


def container_name(app):
    """Resolve the running container name for an app via `docker compose ps`."""
    d = app_dir(app)
    if not d:
        return None
    compose = os.path.join(d, "docker-compose.yml")
    try:
        proc = subprocess.run(
            ["docker", "compose", "-f", compose, "ps", "--format", "{{.Name}}", "app"],
            capture_output=True, text=True, timeout=15,
            env={**os.environ, "DEPLOY_IMAGE": "placeholder"})
        name = (proc.stdout or "").strip().splitlines()
        return name[0] if name else None
    except (subprocess.TimeoutExpired, OSError):
        return None


def docker_logs(app):
    cname = container_name(app)
    if not cname:
        return "нет контейнера для этого приложения"
    try:
        proc = subprocess.run(
            ["docker", "logs", "--tail", "30", cname],
            capture_output=True, text=True, timeout=20)
        out = (proc.stdout or "") + (proc.stderr or "")
        return out.strip() or "(пустой вывод логов)"
    except (subprocess.TimeoutExpired, OSError) as e:
        return f"docker logs failed: {e}"


def current_sha(app):
    """Read current= from the app's .deploy-state (full sha-<hex> tag)."""
    d = app_dir(app)
    if not d:
        return None
    try:
        with open(os.path.join(d, ".deploy-state"), encoding="utf-8") as fh:
            for line in fh:
                if line.startswith("current="):
                    return line.strip()[len("current="):]
    except OSError:
        return None
    return None


def live_url(app):
    """Stable HTTPS URL for an app (WB2): Caddy serves every app at
    https://<app>.<HOST_SLUG>.sslip.io with an auto Let's Encrypt cert."""
    return f"https://{app}.{HOST_SLUG}.sslip.io"


def status_rows():
    """Parse `runner.sh status` into per-app dicts."""
    code, out = run_runner(["status"])
    if code != 0:
        return None, out
    rows = {}
    for line in out.splitlines()[1:]:  # skip runner's header
        parts = [p.strip() for p in line.split("|")]
        if len(parts) < 4 or not parts[0]:
            continue
        rows[parts[0]] = {"sha": parts[1], "health": parts[2], "last": parts[3]}
    return rows, None


def health_glyph(health):
    """Map runner's `state/hs` health string to a consistent glyph."""
    h = (health or "").lower()
    if "healthy" in h or "/ok" in h:
        return GLYPH_HEALTHY
    if h.startswith("running"):
        return GLYPH_RUNNING
    return GLYPH_DOWN


def health_verdict(health):
    """One-line verdict for the app card (RS-05: read the state in 1 second)."""
    g = health_glyph(health)
    if g == GLYPH_HEALTHY:
        return V_HEALTHY
    if g == GLYPH_RUNNING:
        return V_WARNING
    return V_DOWN


def deploy_number(app):
    """Count of successful deploys for the app, from the journal (for the card)."""
    try:
        with open(LOG_FILE, encoding="utf-8") as fh:
            return sum(1 for ln in fh
                       if f"] {app}@" in ln and " deploy ok " in ln)
    except OSError:
        return 0


# --- server resources (#4) ----------------------------------------------------
def server_resources():
    """Snapshot of host RAM and disk. Values are read from /proc and statvfs so
    no shell is spawned. Returns a dict with derived MiB / percent fields."""
    res = {"ram_total": 0, "ram_used": 0, "ram_avail": 0,
           "disk_pct": 0, "disk_free": "?", "uptime": "?"}
    try:
        meminfo = {}
        with open("/proc/meminfo", encoding="utf-8") as fh:
            for line in fh:
                k, _, v = line.partition(":")
                meminfo[k.strip()] = int(v.strip().split()[0])  # kB
        total = meminfo.get("MemTotal", 0) // 1024
        avail = meminfo.get("MemAvailable", 0) // 1024
        res["ram_total"] = total
        res["ram_avail"] = avail
        res["ram_used"] = total - avail
    except (OSError, ValueError):
        pass
    try:
        st = os.statvfs("/")
        total_b = st.f_blocks * st.f_frsize
        free_b = st.f_bavail * st.f_frsize
        used_b = total_b - free_b
        res["disk_pct"] = round(used_b * 100 / total_b) if total_b else 0
        res["disk_free"] = _human_bytes(free_b)
    except OSError:
        pass
    try:
        with open("/proc/uptime", encoding="utf-8") as fh:
            secs = int(float(fh.read().split()[0]))
        res["uptime"] = _human_uptime(secs)
    except (OSError, ValueError):
        pass
    return res


def _human_bytes(n):
    for unit in ("B", "K", "M", "G", "T"):
        if n < 1024 or unit == "T":
            return f"{n:.0f}{unit}" if unit in ("B", "K") else f"{n:.1f}{unit}"
        n /= 1024


def _human_uptime(secs):
    d, rem = divmod(secs, 86400)
    h, rem = divmod(rem, 3600)
    m, _ = divmod(rem, 60)
    if d:
        return f"{d}d {h}h"
    if h:
        return f"{h}h {m}m"
    return f"{m}m"


def container_count():
    """Number of running docker containers (best-effort)."""
    try:
        proc = subprocess.run(["docker", "ps", "-q"],
                              capture_output=True, text=True, timeout=10)
        return len([x for x in proc.stdout.splitlines() if x.strip()])
    except (subprocess.TimeoutExpired, OSError):
        return "?"


def resource_line():
    """One-line RAM+disk summary reused in /status and app cards (#4)."""
    r = server_resources()
    return T_RES_LINE.format(
        ram_used=r["ram_used"], ram_total=r["ram_total"], ram_avail=r["ram_avail"],
        disk_pct=r["disk_pct"], disk_free=r["disk_free"])


# --- background monitor: health state per app (#2) ----------------------------
def app_health_state(app):
    """Coarse health of an app's container for the monitor: one of
    'healthy' | 'unhealthy' | 'running' | 'exited' | 'missing'. Uses docker
    inspect directly so it sees exited/unhealthy transitions the moment they
    happen (independent of the runner status formatting)."""
    cname = container_name(app)
    if not cname:
        return "missing"
    try:
        proc = subprocess.run(
            ["docker", "inspect", "--format",
             "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}",
             cname],
            capture_output=True, text=True, timeout=10)
        out = (proc.stdout or "").strip()
        if not out or proc.returncode != 0:
            return "missing"
        status, _, health = out.partition("|")
        if health == "unhealthy":
            return "unhealthy"
        if health == "healthy":
            return "healthy"
        if status == "running":
            return "running"
        if status == "exited":
            return "exited"
        return status or "missing"
    except (subprocess.TimeoutExpired, OSError):
        return "missing"


# a state is "bad" (deserves an alert) when it is not a healthy/running signal
BAD_STATES = {"unhealthy", "exited", "missing"}


# --- screens (each returns (text, reply_markup)) ------------------------------
def esc(s):
    return html.escape(str(s), quote=False)


def screen_menu():
    kb = {"inline_keyboard": [
        [{"text": B_APPS, "callback_data": "apps"},
         {"text": B_STATUS, "callback_data": "status"}],
        [{"text": B_CONNECT, "callback_data": "conn:0"}],
        [{"text": B_SERVER, "callback_data": "server"},
         {"text": B_HELP, "callback_data": "help"}],
    ]}
    return T_MENU + "\n" + T_MENU_HINT, kb


def screen_server():
    r = server_resources()
    text = T_SERVER_BODY.format(
        title=T_SERVER_TITLE,
        ram_used=r["ram_used"], ram_total=r["ram_total"], ram_avail=r["ram_avail"],
        disk_pct=r["disk_pct"], disk_free=r["disk_free"], uptime=r["uptime"],
        containers=container_count())
    kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
    return text, kb


def screen_help():
    kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
    return T_HELP, kb


# --- Connect-a-repo screens (WB4) ---------------------------------------------
# Repo list is cached briefly so pagination does not re-hit GitHub on every tap.
_REPO_CACHE = {"at": 0, "repos": []}
_CONN_PAGE = 8


def connectable_repos():
    """Owner repos that are not yet onboarded, newest first (cached ~60 s)."""
    now = time.time()
    if now - _REPO_CACHE["at"] > 60:
        token, owner = load_github_env()
        repos = gh_list_repos(token, owner) if token and owner else []
        _REPO_CACHE["at"] = now
        _REPO_CACHE["repos"] = repos
    connected = set(list_apps())
    # hide already-connected repos (their lowercased name is the app name)
    return [r for r in _REPO_CACHE["repos"]
            if r["name"].lower() not in connected]


def screen_connect(page):
    repos = connectable_repos()
    if repos is None:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
        return C_GH_FAIL, kb
    if not repos:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
        return C_NONE, kb
    start = page * _CONN_PAGE
    chunk = repos[start:start + _CONN_PAGE]
    buttons = [[{"text": r["name"], "callback_data": f"crepo:{r['name']}"}]
               for r in chunk]
    nav = []
    if start + _CONN_PAGE < len(repos):
        nav.append({"text": B_MORE, "callback_data": f"conn:{page + 1}"})
    nav.append({"text": B_BACK, "callback_data": "menu"})
    buttons.append(nav)
    return C_TITLE, {"inline_keyboard": buttons}


def screen_connect_detected(repo):
    """One-tap path: detect the profile from repo contents and present the
    result for confirmation, with a manual override. Returns (text, kb, profile);
    profile is None when there is no Dockerfile (the caller refuses)."""
    token, owner = load_github_env()
    branch = "main"
    if token and owner:
        status, meta = gh_api(token, "GET", f"/repos/{owner}/{repo}")
        if status == 200 and isinstance(meta, dict):
            branch = meta.get("default_branch", "main")
        profile, _cport, reason = detect_profile(token, owner, repo, branch)
    else:
        profile, reason = None, E_NO_GH

    if profile is None:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "conn:0"}]]}
        return C_FAIL.format(esc(repo), esc(reason)), kb, None

    tmpl = C_DETECT_SERVICE if profile == "service" else C_DETECT_BOT
    kb = {"inline_keyboard": [
        [{"text": B_DO_CONNECT, "callback_data": f"cgo:{profile}:{repo}"}],
        [{"text": B_CHANGE_TYPE, "callback_data": f"ctype:{repo}"},
         {"text": B_CANCEL, "callback_data": "conn:0"}],
    ]}
    return tmpl.format(esc(repo), esc(reason)), kb, profile


def screen_connect_profile(repo):
    """Manual override menu (behind "Change type")."""
    kb = {"inline_keyboard": [
        [{"text": B_ST_STATIC, "callback_data": f"cprof:static:{repo}"},
         {"text": B_ST_SERVICE, "callback_data": f"cprof:service:{repo}"},
         {"text": B_ST_BOT, "callback_data": f"cprof:bot:{repo}"}],
        [{"text": B_BACK, "callback_data": f"crepo:{repo}"}],
    ]}
    return C_PICK_PROFILE.format(esc(repo)), kb


def screen_connect_confirm(profile, repo):
    kb = {"inline_keyboard": [[
        {"text": B_DO_CONNECT, "callback_data": f"cgo:{profile}:{repo}"},
        {"text": B_CANCEL, "callback_data": f"crepo:{repo}"}]]}
    return C_CONFIRM.format(esc(repo), esc(profile)), kb


_APPS_PAGE = 6  # RS-05: paginate lists longer than ~6


def screen_apps(page=0):
    apps = list_apps()
    if not apps:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
        return T_NO_APPS, kb
    rows, _ = status_rows()
    rows = rows or {}
    total_pages = (len(apps) + _APPS_PAGE - 1) // _APPS_PAGE
    page = max(0, min(page, total_pages - 1))
    start = page * _APPS_PAGE
    chunk = apps[start:start + _APPS_PAGE]
    buttons, row = [], []
    for app in chunk:
        g = health_glyph(rows.get(app, {}).get("health", ""))
        row.append({"text": f"{g} {app}", "callback_data": f"app:{app}"})
        if len(row) == 2:
            buttons.append(row)
            row = []
    if row:
        buttons.append(row)
    # page nav [◀ i/N ▶] only when there is more than one page
    if total_pages > 1:
        nav = []
        if page > 0:
            nav.append({"text": "◀", "callback_data": f"apage:{page - 1}"})
        nav.append({"text": f"{page + 1}/{total_pages}", "callback_data": "noop"})
        if page < total_pages - 1:
            nav.append({"text": "▶", "callback_data": f"apage:{page + 1}"})
        buttons.append(nav)
    buttons.append([{"text": B_BACK, "callback_data": "menu"}])
    title = f"\U0001f4e6 <b>{CRUMB_APPS}</b>"
    if total_pages > 1:
        title += f" ({page + 1}/{total_pages})"
    return title + "\n" + T_APPS_HINT, {"inline_keyboard": buttons}


def screen_app(app):
    """App card, RS-05 checklist: status glyph + breadcrumb title, health in
    <code>, bullet metrics, a clickable URL, and a one-line verdict."""
    rows, err = status_rows()
    if err is not None:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "apps"}]]}
        return T_RUNNER_FAIL.format(1, esc(err)), kb
    info = (rows or {}).get(app, {})
    health = info.get("health", "-")
    g = health_glyph(health)
    url = live_url(app)
    n = deploy_number(app)
    # URL line: clickable link for web apps, a plain note for bots (no URL)
    if url and url != "-":
        url_line = f"\U0001f517 <a href=\"{esc(url)}\">{T_CARD_URL_LABEL}</a>"
    else:
        url_line = f"\U0001f517 {T_CARD_NO_URL}"
    text = (
        f"{g} <b>{CRUMB_APPS} › {esc(app)}</b>\n"
        f"<code>{esc(health)}</code>\n\n"
        f"\U0001f4ca <b>Метрики</b>\n"
        f"  • sha: <code>{esc(info.get('sha', '-'))}</code>\n"
        f"  • деплой #{n} • {esc(info.get('last', 'never'))}\n\n"
        f"{url_line}\n\n"
        f"{health_verdict(health)}"
    )
    kb = {"inline_keyboard": [
        # read + navigate first; the destructive rollback sits on its own row
        [{"text": B_LOGS, "callback_data": f"logs:{app}"},
         {"text": B_REFRESH, "callback_data": f"app:{app}"}],
        [{"text": B_REDEPLOY, "callback_data": f"rdq:{app}"}],
        [{"text": B_ROLLBACK, "callback_data": f"rbq:{app}"}],
        [{"text": B_BACK, "callback_data": "apps"}],
    ]}
    return text, kb


def screen_confirm(app, kind):
    title = T_CONFIRM_ROLLBACK if kind == "rb" else T_CONFIRM_REDEPLOY
    yes = f"{'rbdo' if kind == 'rb' else 'rddo'}:{app}"
    kb = {"inline_keyboard": [[
        {"text": B_YES, "callback_data": yes},
        {"text": B_CANCEL, "callback_data": f"app:{app}"}]]}
    return title.format(esc(app)), kb


def screen_status():
    rows, err = status_rows()
    kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
    if err is not None:
        return T_RUNNER_FAIL.format(1, esc(err)), kb
    lines = [f"\U0001f4ca <b>{CRUMB_STATUS}</b>", ""]
    for app, info in (rows or {}).items():
        g = health_glyph(info["health"])
        lines.append(
            f"{g} <b>{esc(app)}</b> <code>{esc(info['sha'])}</code>\n"
            f"    {esc(live_url(app))}")
    lines.append("")
    lines.append(resource_line())  # #4: server RAM + disk footer
    return "\n".join(lines), kb


# --- actions (perform + return a toast for answerCallbackQuery) ---------------
def do_rollback(app):
    # no tag -> runner journals this as a real rollback to previous sha
    code, _ = run_runner(["rollback", app])
    return code == 0


def do_redeploy(app):
    cur = current_sha(app)
    if not cur:
        return False
    # explicit current tag -> runner journals this as a redeploy
    code, _ = run_runner(["rollback", app, cur])
    return code == 0


# --- slash-command fallback ---------------------------------------------------
def handle_message(api, chat_id, text):
    parts = text.strip().split()
    if not parts:
        return
    cmd = parts[0].split("@")[0].lower()  # strip @botname suffix
    arg = parts[1] if len(parts) > 1 else None

    if cmd in ("/start", "/menu"):
        t, kb = screen_menu()
        api.send(chat_id, t, kb)
    elif cmd == "/help":
        api.send(chat_id, T_HELP)
    elif cmd == "/status":
        t, _ = screen_status()
        api.send(chat_id, t)
    elif cmd == "/server":
        t, _ = screen_server()
        api.send(chat_id, t)
    elif cmd == "/apps":
        t, kb = screen_apps()
        api.send(chat_id, t, kb)
    elif cmd in ("/logs", "/history", "/rollback", "/redeploy"):
        if not arg:
            api.send(chat_id, T_NEED_APP.format(cmd))
            return
        if not valid_app(arg):
            api.send(chat_id, T_UNKNOWN_APP.format(esc(arg)))
            return
        if cmd == "/logs":
            send_logs(api, chat_id, arg)
        elif cmd == "/history":
            code, out = run_runner(["history", arg])
            api.send(chat_id, f"<b>history {esc(arg)}</b>\n<pre>{esc(out)[-3500:]}</pre>")
        elif cmd == "/rollback":
            t, kb = screen_confirm(arg, "rb")
            api.send(chat_id, t, kb)
        elif cmd == "/redeploy":
            t, kb = screen_confirm(arg, "rd")
            api.send(chat_id, t, kb)
    else:
        api.send(chat_id, T_UNKNOWN_CMD)


def send_logs(api, chat_id, app):
    out = docker_logs(app)
    body = esc(out)[-3500:]
    api.send(chat_id, f"<b>logs {esc(app)}</b>\n<pre>{body}</pre>")


# --- callback routing (edits the same message) --------------------------------
REPO_RE = re.compile(r"^[A-Za-z0-9._-]{1,100}$")


def handle_callback(api, chat_id, message_id, cb_id, data):
    action, _, rest = data.partition(":")

    # --- WB4 "Connect a repo" branches (carry repo/profile, not an app) -------
    if action in ("conn", "crepo", "ctype", "cprof", "cgo"):
        _handle_connect(api, chat_id, message_id, cb_id, action, rest)
        return

    # page navigation / inert buttons carry a number or nothing (not an app)
    if action == "noop":
        api.answer_callback(cb_id)  # inert page counter: just close the spinner
        return
    if action == "apage":
        page = int(rest) if rest.isdigit() else 0
        _nav(api, chat_id, message_id, cb_id, *screen_apps(page))
        return

    app = rest
    # every app-carrying action re-validates it against the allowlist
    if app and not valid_app(app):
        api.answer_callback(cb_id, T_UNKNOWN_APP.format(app))
        return

    if action == "menu":
        _nav(api, chat_id, message_id, cb_id, *screen_menu())
    elif action == "help":
        _nav(api, chat_id, message_id, cb_id, *screen_help())
    elif action == "apps":
        _nav(api, chat_id, message_id, cb_id, *screen_apps())
    elif action == "status":
        _nav(api, chat_id, message_id, cb_id, *screen_status())
    elif action == "server":
        _nav(api, chat_id, message_id, cb_id, *screen_server())
    elif action == "app":
        _nav(api, chat_id, message_id, cb_id, *screen_app(app), toast=T_REFRESHED)
    elif action == "logs":
        api.answer_callback(cb_id)
        send_logs(api, chat_id, app)  # long -> separate message, by design
    elif action == "rbq":
        _nav(api, chat_id, message_id, cb_id, *screen_confirm(app, "rb"))
    elif action == "rdq":
        _nav(api, chat_id, message_id, cb_id, *screen_confirm(app, "rd"))
    elif action == "rbdo":
        _perform(api, chat_id, message_id, cb_id, app, do_rollback, T_DONE_ROLLBACK)
    elif action == "rddo":
        _perform(api, chat_id, message_id, cb_id, app, do_redeploy, T_DONE_REDEPLOY)
    else:
        api.answer_callback(cb_id)


def _nav(api, chat_id, message_id, cb_id, text, kb, toast=None):
    api.answer_callback(cb_id, toast)
    api.edit(chat_id, message_id, text, kb)


def _perform(api, chat_id, message_id, cb_id, app, fn, done_msg):
    # RS-05 loading state: close the spinner with a toast AND show a visible
    # "working" message so a slow deploy never looks frozen, then the result
    api.answer_callback(cb_id, T_WORKING)
    api.edit(chat_id, message_id, T_LOADING, None)
    ok = fn(app)
    # refresh the card with the post-action status + a short banner
    text, kb = screen_app(app)
    banner = ("\U0001f680 " + done_msg) if ok else ("\U0001f534 " + T_FAILED)
    api.edit(chat_id, message_id, banner + "\n\n" + text, kb)


def _handle_connect(api, chat_id, message_id, cb_id, action, rest):
    """Route the Connect-a-repo flow. Repo/profile args are validated here."""
    if action == "conn":
        page = int(rest) if rest.isdigit() else 0
        _nav(api, chat_id, message_id, cb_id, *screen_connect(page))
        return

    # crepo / ctype carry just <repo>
    if action in ("crepo", "ctype"):
        repo = rest
        if not REPO_RE.match(repo):
            api.answer_callback(cb_id, C_NONE)
            return
        # idempotency: an already-onboarded repo has no further action
        if repo.lower() in set(list_apps()):
            _nav(api, chat_id, message_id, cb_id, C_ALREADY.format(esc(repo)),
                 {"inline_keyboard": [[{"text": B_BACK, "callback_data": "conn:0"}]]})
            return
        if action == "ctype":
            # manual override: show the static/service/bot menu
            _nav(api, chat_id, message_id, cb_id, *screen_connect_profile(repo))
        else:
            # default one-tap path: auto-detect and present the result
            api.answer_callback(cb_id)
            text, kb, _profile = screen_connect_detected(repo)
            api.edit(chat_id, message_id, text, kb)
        return

    # cprof / cgo carry "<profile>:<repo>"
    profile, _, repo = rest.partition(":")
    if profile not in PROFILE_DEFAULTS or not REPO_RE.match(repo):
        api.answer_callback(cb_id, C_NONE)
        return

    if action == "cprof":
        _nav(api, chat_id, message_id, cb_id, *screen_connect_confirm(profile, repo))
        return

    if action == "cgo":
        _do_connect(api, chat_id, message_id, cb_id, profile, repo)


def _do_connect(api, chat_id, message_id, cb_id, profile, repo):
    """Execute onboarding after the explicit confirm (destructive: writes to the
    repo). Streams step lines by editing the same message; ends with a result
    card + the app's HTTPS URL."""
    app = repo.lower()
    # guard the destructive path again right before acting
    if app in set(list_apps()):
        api.answer_callback(cb_id, C_ALREADY.format(repo))
        _nav(api, chat_id, message_id, cb_id, C_ALREADY.format(esc(repo)),
             {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]})
        return
    api.answer_callback(cb_id, T_WORKING)
    steps = []

    def progress(msg):
        steps.append(C_STEP.format(esc(msg)))
        api.edit(chat_id, message_id,
                 C_WORKING.format(esc(repo)) + "\n" + "\n".join(steps))

    try:
        ok, info = wb4_onboard(app, repo, profile, progress=progress)
    except Exception as e:  # onboarding must never crash the bot
        bot_log(f"wb4_onboard error: {type(e).__name__}: {e}")
        ok, info = False, str(e)

    back = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
    if ok:
        branch = info
        text = C_OK.format(esc(app), esc(app), HOST_SLUG, esc(branch))
    else:
        text = C_FAIL.format(esc(repo), esc(info))
    api.edit(chat_id, message_id, text, back)


# --- background monitor (#2) --------------------------------------------------
class Monitor:
    """Samples app health + host resources on a timer and messages ONLY the
    owner on a state CHANGE (edge-triggered), never on every tick. Resource
    alerts have a cooldown so they do not spam while the condition persists.

    Runs in a daemon thread inside the same process as the poll loop. Deploy
    failures are intentionally NOT re-reported here — runner already sends those
    cards. A failure inside the monitor is caught and logged; it never takes the
    bot down.
    """

    def __init__(self, api, owner_chat_id):
        self._api = api
        self._owner = owner_chat_id
        self._health = {}          # app -> last known state
        self._disk_alerted_at = 0  # epoch of last disk alert (0 = not alerting)
        self._ram_alerted_at = 0

    def _notify(self, text):
        # monitor messages go to the owner and nobody else
        self._api.send(self._owner, text)
        bot_log(f"ALERT {text}")

    def _check_health(self):
        for app in list_apps():
            cur = app_health_state(app)
            prev = self._health.get(app)
            self._health[app] = cur
            if prev is None:
                continue  # first observation: establish baseline, no alert
            if prev == cur:
                continue
            cur_bad = cur in BAD_STATES
            prev_bad = prev in BAD_STATES
            if cur_bad and not prev_bad:
                self._notify(A_UNHEALTHY.format(esc(app), esc(cur)))
            elif prev_bad and not cur_bad:
                self._notify(A_RECOVERED.format(esc(app)))

    def _check_resources(self, conf):
        now = time.time()
        r = server_resources()
        # disk: alert above threshold with cooldown; reset when it drops back
        if r["disk_pct"] > conf["disk_pct"]:
            if now - self._disk_alerted_at >= conf["cooldown_sec"]:
                self._notify(A_DISK.format(r["disk_pct"], r["disk_free"]))
                self._disk_alerted_at = now
        elif self._disk_alerted_at:
            self._notify(A_DISK_OK.format(r["disk_pct"]))
            self._disk_alerted_at = 0
        # RAM: alert below threshold with cooldown; reset when it recovers
        if r["ram_avail"] and r["ram_avail"] < conf["ram_min_mib"]:
            if now - self._ram_alerted_at >= conf["cooldown_sec"]:
                self._notify(A_RAM.format(r["ram_avail"]))
                self._ram_alerted_at = now
        elif self._ram_alerted_at:
            self._notify(A_RAM_OK.format(r["ram_avail"]))
            self._ram_alerted_at = 0

    def tick(self):
        """One monitoring pass. Isolated so tests can drive it directly."""
        conf = load_alerts_conf()
        try:
            self._check_health()
        except Exception as e:  # a monitor must never crash the bot
            bot_log(f"monitor health error: {type(e).__name__}: {e}")
        try:
            self._check_resources(conf)
        except Exception as e:
            bot_log(f"monitor resource error: {type(e).__name__}: {e}")

    def run_forever(self):
        while True:
            interval = load_alerts_conf()["interval_sec"]
            self.tick()
            time.sleep(max(10, interval))


# the "/" quick-command menu (setMyCommands): <=6 commands with emoji (RS-05)
MY_COMMANDS = [
    {"command": "menu",   "description": "\U0001f3e0 Главное меню"},
    {"command": "status", "description": "\U0001f4ca Статус приложений"},
    {"command": "apps",   "description": "\U0001f4e6 Список приложений"},
    {"command": "server", "description": "\U0001f5a5 Ресурсы сервера"},
    {"command": "help",   "description": "❔ Справка"},
]


# --- main loop ----------------------------------------------------------------
def main():
    token, authorized_chat_id = load_config()
    api = Api(token)
    bot_log(f"bot started; authorized_chat_id={authorized_chat_id}")
    # register the "/" command menu (RS-05: second entry point after buttons)
    api.set_commands(MY_COMMANDS)
    # background monitor: edge-triggered alerts to the owner only (#2)
    monitor = Monitor(api, authorized_chat_id)
    threading.Thread(target=monitor.run_forever, daemon=True).start()
    offset = 0
    while True:
        for update in api.get_updates(offset):
            offset = update["update_id"] + 1
            msg = update.get("message")
            cb = update.get("callback_query")
            # --- P0 GATE: act ONLY for the authorized chat AND user ----------
            if msg:
                chat_id = (msg.get("chat") or {}).get("id")
                from_id = (msg.get("from") or {}).get("id")
                if chat_id != authorized_chat_id or from_id != authorized_chat_id:
                    uname = (msg.get("from") or {}).get("username", "?")
                    log_denied(chat_id, uname)
                    continue
                text = msg.get("text")
                if text:
                    handle_message(api, chat_id, text)
            elif cb:
                cb_msg = cb.get("message") or {}
                cb_chat = (cb_msg.get("chat") or {}).get("id")
                from_id = (cb.get("from") or {}).get("id")
                # both the chat and the pressing user must be authorized
                if cb_chat != authorized_chat_id or from_id != authorized_chat_id:
                    uname = (cb.get("from") or {}).get("username", "?")
                    log_denied(from_id, uname)
                    api.answer_callback(cb.get("id", ""))
                    continue
                handle_callback(api, cb_chat, cb_msg.get("message_id"),
                                cb.get("id", ""), cb.get("data", ""))


if __name__ == "__main__":
    main()
