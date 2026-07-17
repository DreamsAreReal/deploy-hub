#!/usr/bin/env python3
"""deploy-hub Telegram control bot (long-polling, stdlib only).

A read+control surface over the existing runner.sh on this VPS. It exposes a
FIXED set of commands (status / apps / logs / rollback / redeploy / history /
help) and never runs arbitrary shell. All privileged work is delegated to
runner.sh, which owns the app allowlist and the deploy logic — the bot adds no
new way around that allowlist.

SECURITY (P0): every update is gated on chat.id == AUTHORIZED_CHAT_ID. Any
other chat is ignored and the attempt (chat_id, username) is journaled. There
is no code path that acts on an unauthorized chat.

Runs as a systemd service under user `deploy` (already in the docker group and
able to read the cloudflared tunnel log). No inbound port: it long-polls
getUpdates. The token is read from telegram.env (600, owner deploy) and is
never placed in argv, git, or logs.
"""
import html
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# --- configuration ------------------------------------------------------------
HUB_DIR = os.environ.get("HUB_DIR", "/opt/deploy-hub")
TELEGRAM_ENV = os.path.join(HUB_DIR, "telegram.env")
RUNNER = os.path.join(HUB_DIR, "bin", "runner.sh")
APPS_LIST = os.path.join(HUB_DIR, "apps.list")
BOT_LOG = os.path.join(HUB_DIR, "bot.log")
# cloudflared quick-tunnel writes its public URL here (StandardOutput=append);
# the URL rotates on every restart, so it is read live on each request (WB2)
TUNNEL_LOG = "/var/log/codex-tunnel.log"
# apps served behind nginx on :80 have a stable path URL (WB2)
NGINX_PATHS = {"portfolio": "/portfolio/", "zhaba": "/zhaba/", "vote": "/vote/"}
NGINX_HOST = "http://192.3.94.42"
# app whose live URL is the rotating cloudflared tunnel
TUNNEL_APP = "codeapp"

APP_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,40}$")
TUNNEL_URL_RE = re.compile(r"https://[a-z0-9-]+\.trycloudflare\.com")
TELEGRAM_TEXT_LIMIT = 4096

# --- user-facing strings (en) -------------------------------------------------
S_HELP = (
    "<b>deploy-hub bot</b>\n"
    "/status — all apps (app | sha | health | url | last deploy)\n"
    "/apps — pick an app for actions\n"
    "/logs &lt;app&gt; — last container log lines\n"
    "/history &lt;app&gt; — deploy journal tail\n"
    "/rollback &lt;app&gt; — roll back to previous sha (confirm)\n"
    "/redeploy &lt;app&gt; — redeploy current sha (confirm)\n"
    "/help — this message"
)
S_UNKNOWN_CMD = "Unknown command. Send /help for the list."
S_NEED_APP = "Usage: {} &lt;app&gt;. Send /apps to see the list."
S_UNKNOWN_APP = "Unknown app: <code>{}</code>. Send /apps for the allowlist."
S_NO_APPS = "No apps registered yet."
S_PICK_APP = "Pick an app:"
S_CONFIRM_ROLLBACK = "Roll back <b>{}</b> to its previous sha? This is destructive."
S_CONFIRM_REDEPLOY = "Redeploy <b>{}</b> at its current sha?"
S_WORKING = "Working on <b>{} {}</b>…"
S_CANCELLED = "Cancelled."
S_RUNNER_FAIL = "runner error (exit {}):\n<pre>{}</pre>"


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


# --- journaling ---------------------------------------------------------------
def bot_log(msg):
    ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    try:
        with open(BOT_LOG, "a", encoding="utf-8") as fh:
            fh.write(f"[{ts}] {msg}\n")
    except OSError:
        pass  # logging must never crash the poll loop


def log_denied(update):
    """P0 audit trail: record a rejected update's chat_id + username. Never
    echoes message text (could contain anything)."""
    src = update.get("message") or update.get("callback_query") or {}
    chat = (src.get("message", src).get("chat") or {}) if "message" in src else (src.get("chat") or {})
    frm = src.get("from") or {}
    cid = chat.get("id", frm.get("id", "?"))
    uname = frm.get("username", frm.get("first_name", "?"))
    bot_log(f"DENIED chat_id={cid} username={uname}")


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
        text = text[:TELEGRAM_TEXT_LIMIT]
        params = {"chat_id": chat_id, "text": text, "parse_mode": "HTML",
                  "disable_web_page_preview": "true"}
        if reply_markup is not None:
            params["reply_markup"] = json.dumps(reply_markup)
        try:
            return self._call("sendMessage", params, timeout=15)
        except (urllib.error.URLError, TimeoutError) as e:
            bot_log(f"sendMessage error: {type(e).__name__}")
            return None

    def answer_callback(self, cb_id):
        try:
            self._call("answerCallbackQuery", {"callback_query_id": cb_id}, timeout=10)
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


def docker_logs(app):
    """Tail the app's container logs. Container name is resolved via runner's
    registry indirectly: we use the app name as container name is not always
    equal, so ask docker by the compose service is out of scope — the app dir's
    container is what `status` reflects. We resolve the container name here."""
    cname = container_name(app)
    if not cname:
        return "no container for this app"
    try:
        proc = subprocess.run(
            ["docker", "logs", "--tail", "30", cname],
            capture_output=True, text=True, timeout=20)
        out = (proc.stdout or "") + (proc.stderr or "")
        return out.strip() or "(no log output)"
    except (subprocess.TimeoutExpired, OSError) as e:
        return f"docker logs failed: {e}"


def container_name(app):
    """Resolve the running container name for an app via `docker compose ps`,
    reusing the app dir from apps.list."""
    app_dir = None
    try:
        with open(APPS_LIST, encoding="utf-8") as fh:
            for line in fh:
                parts = line.split()
                if parts and parts[0] == app:
                    app_dir = parts[1] if len(parts) > 1 else f"/opt/{app}"
                    break
    except OSError:
        return None
    if not app_dir:
        return None
    compose = os.path.join(app_dir, "docker-compose.yml")
    try:
        proc = subprocess.run(
            ["docker", "compose", "-f", compose, "ps", "--format", "{{.Name}}", "app"],
            capture_output=True, text=True, timeout=15,
            env={**os.environ, "DEPLOY_IMAGE": "placeholder"})
        name = (proc.stdout or "").strip().splitlines()
        return name[0] if name else None
    except (subprocess.TimeoutExpired, OSError):
        return None


def live_url(app):
    """Current working URL for an app (WB2). nginx-proxied apps have a stable
    path; the tunnel app's URL is read live from the cloudflared log so it is
    always the current one even after a tunnel restart."""
    if app in NGINX_PATHS:
        return NGINX_HOST + NGINX_PATHS[app]
    if app == TUNNEL_APP:
        return tunnel_url() or "(tunnel url unavailable)"
    return "-"


def tunnel_url():
    """Last cloudflared quick-tunnel URL from the log = the current one."""
    try:
        with open(TUNNEL_LOG, encoding="utf-8", errors="replace") as fh:
            found = TUNNEL_URL_RE.findall(fh.read())
        return found[-1] if found else None
    except OSError:
        return None


# --- command handlers ---------------------------------------------------------
def esc(s):
    return html.escape(s, quote=False)


def handle_status(api, chat_id):
    code, out = run_runner(["status"])
    if code != 0:
        api.send(chat_id, S_RUNNER_FAIL.format(code, esc(out)))
        return
    lines = out.splitlines()
    rows = ["<b>app | sha | health | url | last deploy</b>"]
    for line in lines[1:]:  # skip runner's own header
        parts = [p.strip() for p in line.split("|")]
        if not parts or not parts[0]:
            continue
        app = parts[0]
        url = live_url(app)
        parts.insert(3, url)
        rows.append(esc(" | ".join(parts)))
    api.send(chat_id, "\n".join(rows))


def handle_apps(api, chat_id):
    apps = list_apps()
    if not apps:
        api.send(chat_id, S_NO_APPS)
        return
    buttons = [[{"text": a, "callback_data": f"app:{a}"}] for a in apps]
    api.send(chat_id, S_PICK_APP, reply_markup={"inline_keyboard": buttons})


def app_action_menu(app):
    return {"inline_keyboard": [
        [{"text": "Status", "callback_data": f"status:{app}"},
         {"text": "Logs", "callback_data": f"logs:{app}"}],
        [{"text": "History", "callback_data": f"history:{app}"}],
        [{"text": "Rollback", "callback_data": f"rbq:{app}"},
         {"text": "Redeploy", "callback_data": f"rdq:{app}"}],
    ]}


def handle_logs(api, chat_id, app):
    out = docker_logs(app)
    # trim to keep well under Telegram's limit after HTML wrapping
    body = esc(out)[-3500:]
    api.send(chat_id, f"<b>logs {esc(app)}</b>\n<pre>{body}</pre>")


def handle_history(api, chat_id, app):
    code, out = run_runner(["history", app])
    body = esc(out)[-3500:]
    api.send(chat_id, f"<b>history {esc(app)}</b>\n<pre>{body}</pre>")


def handle_rollback_query(api, chat_id, app):
    kb = {"inline_keyboard": [[
        {"text": "Yes, roll back", "callback_data": f"rbdo:{app}"},
        {"text": "No", "callback_data": "cancel"}]]}
    api.send(chat_id, S_CONFIRM_ROLLBACK.format(esc(app)), reply_markup=kb)


def handle_redeploy_query(api, chat_id, app):
    kb = {"inline_keyboard": [[
        {"text": "Yes, redeploy", "callback_data": f"rddo:{app}"},
        {"text": "No", "callback_data": "cancel"}]]}
    api.send(chat_id, S_CONFIRM_REDEPLOY.format(esc(app)), reply_markup=kb)


def do_rollback(api, chat_id, app):
    api.send(chat_id, S_WORKING.format("rollback", esc(app)))
    # no tag -> runner journals this as a real rollback to previous sha
    code, out = run_runner(["rollback", app])
    api.send(chat_id, f"<b>rollback {esc(app)}</b> (exit {code})\n<pre>{esc(out)[-3500:]}</pre>")


def do_redeploy(api, chat_id, app):
    api.send(chat_id, S_WORKING.format("redeploy", esc(app)))
    # redeploy = rollback to the CURRENT sha (explicit tag -> runner logs redeploy)
    cur = current_sha(app)
    if not cur:
        api.send(chat_id, f"cannot read current sha for <code>{esc(app)}</code>")
        return
    code, out = run_runner(["rollback", app, cur])
    api.send(chat_id, f"<b>redeploy {esc(app)}</b> (exit {code})\n<pre>{esc(out)[-3500:]}</pre>")


def current_sha(app):
    """Read current= from the app's .deploy-state (full sha-<hex> tag)."""
    app_dir = None
    try:
        with open(APPS_LIST, encoding="utf-8") as fh:
            for line in fh:
                parts = line.split()
                if parts and parts[0] == app:
                    app_dir = parts[1] if len(parts) > 1 else f"/opt/{app}"
                    break
    except OSError:
        return None
    if not app_dir:
        return None
    try:
        with open(os.path.join(app_dir, ".deploy-state"), encoding="utf-8") as fh:
            for line in fh:
                if line.startswith("current="):
                    return line.strip()[len("current="):]
    except OSError:
        return None
    return None


# --- dispatch -----------------------------------------------------------------
def handle_message(api, chat_id, text):
    parts = text.strip().split()
    if not parts:
        return
    cmd = parts[0].split("@")[0].lower()  # strip @botname suffix
    arg = parts[1] if len(parts) > 1 else None

    if cmd in ("/help", "/start"):
        api.send(chat_id, S_HELP)
    elif cmd == "/status":
        handle_status(api, chat_id)
    elif cmd == "/apps":
        handle_apps(api, chat_id)
    elif cmd in ("/logs", "/history", "/rollback", "/redeploy"):
        if not arg:
            api.send(chat_id, S_NEED_APP.format(cmd))
            return
        if not valid_app(arg):
            api.send(chat_id, S_UNKNOWN_APP.format(esc(arg)))
            return
        if cmd == "/logs":
            handle_logs(api, chat_id, arg)
        elif cmd == "/history":
            handle_history(api, chat_id, arg)
        elif cmd == "/rollback":
            handle_rollback_query(api, chat_id, arg)
        elif cmd == "/redeploy":
            handle_redeploy_query(api, chat_id, arg)
    else:
        api.send(chat_id, S_UNKNOWN_CMD)


def handle_callback(api, chat_id, data):
    if data == "cancel":
        api.send(chat_id, S_CANCELLED)
        return
    if ":" not in data:
        return
    action, app = data.split(":", 1)
    # every button that carries an app re-validates it against the allowlist
    if action in ("app", "status", "logs", "history", "rbq", "rdq", "rbdo", "rddo"):
        if not valid_app(app):
            api.send(chat_id, S_UNKNOWN_APP.format(esc(app)))
            return
    if action == "app":
        api.send(chat_id, f"<b>{esc(app)}</b> — choose action:",
                 reply_markup=app_action_menu(app))
    elif action == "status":
        handle_status(api, chat_id)
    elif action == "logs":
        handle_logs(api, chat_id, app)
    elif action == "history":
        handle_history(api, chat_id, app)
    elif action == "rbq":
        handle_rollback_query(api, chat_id, app)
    elif action == "rdq":
        handle_redeploy_query(api, chat_id, app)
    elif action == "rbdo":
        do_rollback(api, chat_id, app)
    elif action == "rddo":
        do_redeploy(api, chat_id, app)


# --- main loop ----------------------------------------------------------------
def main():
    token, authorized_chat_id = load_config()
    api = Api(token)
    bot_log(f"bot started; authorized_chat_id={authorized_chat_id}")
    offset = 0
    while True:
        for update in api.get_updates(offset):
            offset = update["update_id"] + 1
            msg = update.get("message")
            cb = update.get("callback_query")
            # --- P0 GATE: act ONLY for the authorized chat -------------------
            if msg:
                chat_id = (msg.get("chat") or {}).get("id")
                if chat_id != authorized_chat_id:
                    log_denied(update)
                    continue
                text = msg.get("text")
                if text:
                    handle_message(api, chat_id, text)
            elif cb:
                cb_chat = ((cb.get("message") or {}).get("chat") or {}).get("id")
                from_id = (cb.get("from") or {}).get("id")
                # both the chat and the pressing user must be authorized
                if cb_chat != authorized_chat_id or from_id != authorized_chat_id:
                    log_denied(update)
                    api.answer_callback(cb.get("id", ""))
                    continue
                api.answer_callback(cb.get("id", ""))
                data = cb.get("data", "")
                if data:
                    handle_callback(api, cb_chat, data)


if __name__ == "__main__":
    main()
