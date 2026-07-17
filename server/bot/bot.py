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
RUNNER = os.path.join(HUB_DIR, "bin", "runner.sh")
APPS_LIST = os.path.join(HUB_DIR, "apps.list")
LOG_FILE = os.path.join(HUB_DIR, "deploys.log")
BOT_LOG = os.path.join(HUB_DIR, "bot.log")
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

# consistent status glyphs (taste: <=1 meaningful glyph per element)
GLYPH_HEALTHY = "✅"   # ✅ healthy / passing gate
GLYPH_RUNNING = "▫️"  # ▫️ running, no health signal
GLYPH_DOWN = "❌"      # ❌ missing / unhealthy

# --- human-facing UI strings (ru) ---------------------------------------------
T_MENU = "<b>deploy-hub</b>\nПанель управления деплоями."
T_HELP = (
    "<b>deploy-hub — помощь</b>\n\n"
    "Управляйте через кнопки меню или командами:\n"
    "/menu — главное меню\n"
    "/status — таблица всех приложений\n"
    "/server — ресурсы сервера (RAM, диск, uptime)\n"
    "/apps — список приложений\n"
    "/logs &lt;app&gt; — хвост логов контейнера\n"
    "/history &lt;app&gt; — журнал деплоев\n"
    "/rollback &lt;app&gt; — откат на прошлый sha\n"
    "/redeploy &lt;app&gt; — передеплой текущего sha\n\n"
    "Откат и передеплой требуют подтверждения."
)
T_APPS_TITLE = "<b>Приложения</b>\nВыберите приложение:"
T_NO_APPS = "Приложений пока нет."
T_UNKNOWN_APP = "Неизвестное приложение: <code>{}</code>"
T_NEED_APP = "Использование: {} &lt;app&gt;"
T_UNKNOWN_CMD = "Неизвестная команда. Откройте /menu."
T_CONFIRM_ROLLBACK = "Откатить <b>{}</b> на предыдущий sha?\nЭто действие изменит прод."
T_CONFIRM_REDEPLOY = "Передеплоить <b>{}</b> на текущий sha?"
T_WORKING = "Выполняю…"
T_CANCELLED = "Отменено"
T_DONE_ROLLBACK = "Откат выполнен"
T_DONE_REDEPLOY = "Передеплой выполнен"
T_FAILED = "Ошибка (см. карточку)"
T_REFRESHED = "Обновлено"
T_RUNNER_FAIL = "runner error (exit {}):\n<pre>{}</pre>"
T_SERVER_TITLE = "<b>Сервер</b>"
T_SERVER_BODY = (
    "{title}\n"
    "RAM: {ram_used}/{ram_total} MiB (avail {ram_avail})\n"
    "диск /: {disk_pct}% занято (свободно {disk_free})\n"
    "uptime: {uptime}\n"
    "контейнеров: {containers}"
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
        [{"text": B_SERVER, "callback_data": "server"},
         {"text": B_HELP, "callback_data": "help"}],
    ]}
    return T_MENU, kb


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


def screen_apps():
    apps = list_apps()
    if not apps:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "menu"}]]}
        return T_NO_APPS, kb
    rows, _ = status_rows()
    rows = rows or {}
    buttons, row = [], []
    for app in apps:
        g = health_glyph(rows.get(app, {}).get("health", ""))
        row.append({"text": f"{g} {app}", "callback_data": f"app:{app}"})
        if len(row) == 2:
            buttons.append(row)
            row = []
    if row:
        buttons.append(row)
    buttons.append([{"text": B_BACK, "callback_data": "menu"}])
    return T_APPS_TITLE, {"inline_keyboard": buttons}


def screen_app(app):
    """App card: name, sha7, health, last deploy + number, live URL."""
    rows, err = status_rows()
    if err is not None:
        kb = {"inline_keyboard": [[{"text": B_BACK, "callback_data": "apps"}]]}
        return T_RUNNER_FAIL.format(1, esc(err)), kb
    info = (rows or {}).get(app, {})
    g = health_glyph(info.get("health", ""))
    url = live_url(app)
    n = deploy_number(app)
    text = (
        f"<b>{esc(app)}</b> {g}\n"
        f"sha: <code>{esc(info.get('sha', '-'))}</code>\n"
        f"health: <code>{esc(info.get('health', '-'))}</code>\n"
        f"деплой #{n} • {esc(info.get('last', 'never'))}\n"
        f"\U0001f517 {esc(url)}\n"
        f"{resource_line()}"  # #4: server RAM + disk
    )
    kb = {"inline_keyboard": [
        [{"text": B_LOGS, "callback_data": f"logs:{app}"},
         {"text": B_ROLLBACK, "callback_data": f"rbq:{app}"}],
        [{"text": B_REDEPLOY, "callback_data": f"rdq:{app}"},
         {"text": B_REFRESH, "callback_data": f"app:{app}"}],
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
    lines = ["<b>Статус приложений</b>"]
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
def handle_callback(api, chat_id, message_id, cb_id, data):
    action, _, app = data.partition(":")

    # every action that carries an app re-validates it against the allowlist
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
    # acknowledge immediately so the button stops spinning, then act
    api.answer_callback(cb_id, T_WORKING)
    ok = fn(app)
    # refresh the card with the post-action status + a short toast
    text, kb = screen_app(app)
    banner = ("✅ " + done_msg) if ok else ("❌ " + T_FAILED)
    api.edit(chat_id, message_id, banner + "\n\n" + text, kb)


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


# --- main loop ----------------------------------------------------------------
def main():
    token, authorized_chat_id = load_config()
    api = Api(token)
    bot_log(f"bot started; authorized_chat_id={authorized_chat_id}")
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
