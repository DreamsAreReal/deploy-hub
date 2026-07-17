# deploy-hub Telegram control bot

An interactive Telegram surface over `runner.sh`, running on the VPS. It lets the
operator check status and drive rollbacks/redeploys from the chat instead of SSH.

The primary UI is a button-driven inline-keyboard menu; navigation edits the same
message in place (no new message per tap). Human-facing UI text is Russian;
identifiers, logs and code stay English.

## What it does

**Buttons (`/start` or `/menu`):**

- Main menu → apps / status / server / help.
- Apps → all apps as buttons (2 per row) with a health glyph (`✅` healthy /
  `▫️` running / `❌` down); back.
- App card → name, sha7, health, deploy number + last-deploy time, live URL, a
  server-resource line (RAM + disk), and actions logs / rollback / redeploy /
  refresh / back. Refresh re-reads status; logs are sent as a separate message.
- Server → host RAM (used/total + available), disk (use% + free), uptime,
  running container count.
- Rollback / Redeploy → inline `Да` / `Отмена` confirm in the same card; after the
  action the card is refreshed with fresh status plus a short toast
  (`answerCallbackQuery`).

**Slash-command fallback (still supported):**

- `/status` — table of all apps (+ RAM/disk footer)
- `/server` — host RAM / disk / uptime / container count
- `/apps` — app buttons
- `/logs <app>` — last ~30 container log lines
- `/history <app>` — deploy journal tail (`runner.sh history`)
- `/rollback <app>` / `/redeploy <app>` — with a Yes/No confirm
- `/menu`, `/help`

## Background monitor (alerts)

A daemon thread in the same process samples app health and host resources on a
timer and messages **only the owner** on a state *change* (edge-triggered, never
per tick). Deploy failures are not re-reported here — `runner.sh` already sends
those cards.

- app health: `healthy/running → unhealthy/exited/missing` → `⚠️ <app> unhealthy`;
  back → `✅ <app> recovered`. Last state is kept in memory.
- disk: `use% > disk_pct` → alert, then a cooldown; drops back below → reset.
- RAM: `available < ram_min_mib` MiB → alert (cooldown); recovers → reset.

Thresholds default to disk 85%, RAM 80 MiB, 60 s interval, 3 h cooldown, and can
be overridden by an optional `/opt/deploy-hub/alerts.conf` (`key=value` lines:
`interval_sec`, `disk_pct`, `ram_min_mib`, `cooldown_sec`). Monitor errors are
caught and logged; they never take the bot down.

The disk `use%` comes from `statvfs` (space available to a normal user), so it
can read a couple of points higher than `df`'s default, which counts the
root-reserved blocks as usable.

The URL (WB2) is the app's stable HTTPS address, served by Caddy with an auto
Let's Encrypt certificate: `https://<app>.192-3-94-42.sslip.io`. The name no
longer moves (it replaced the old nginx `:80` paths and the rotating cloudflared
tunnel).

## Security model (P0)

- Every update is gated on `chat.id == TG_CHAT_ID`. Any other chat is ignored and
  the attempt (`chat_id`, `username`) is journaled to `bot.log`. There is no code
  path that acts for an unauthorized chat. Inline-button presses re-check both the
  chat id and the pressing user's id.
- The bot never runs arbitrary shell. Every privileged action is delegated to
  `runner.sh`, which owns the app allowlist and the deploy logic. App names are
  validated against `apps.list` (the same source runner uses) before use, and
  runner is invoked with an argument **list**, never a shell string.
- The token is read from `telegram.env` (mode 600, owner `deploy`). It is never
  placed in argv, git, environment of child processes, or logs.

## Deployment

Runs as the systemd service `deploy-hub-bot` under user `deploy` (already in the
`docker` group and able to read the tunnel log). `Restart=always`, `MemoryMax=64M`,
enabled so it survives reboot. No inbound port — it long-polls `getUpdates`.

Install:

```
sudo install -m 755 -o deploy -g deploy server/bot/bot.py /opt/deploy-hub/bin/bot.py
sudo install -m 644 server/systemd/deploy-hub-bot.service /etc/systemd/system/
# /opt/deploy-hub is root-owned; pre-create the audit log owned by deploy so the
# denied-attempt trail is never silently lost (bot_log fails soft on OSError)
sudo install -m 644 -o deploy -g deploy /dev/null /opt/deploy-hub/bot.log
sudo systemctl daemon-reload
sudo systemctl enable --now deploy-hub-bot
```

Logs: `journalctl -u deploy-hub-bot`; audit trail of denied chats:
`/opt/deploy-hub/bot.log`.
