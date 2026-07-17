# deploy-hub Telegram control bot

An interactive Telegram surface over `runner.sh`, running on the VPS. It lets the
operator check status and drive rollbacks/redeploys from the chat instead of SSH.

## What it does

Fixed command set (nothing else is executable through the bot):

- `/status` — table of all apps: `app | sha | health | url | last deploy`
- `/apps` — inline buttons, one per app, drilling into per-app actions
- `/logs <app>` — last ~30 container log lines (trimmed to Telegram's limit)
- `/history <app>` — deploy journal tail (`runner.sh history`)
- `/rollback <app>` — roll back to the previous sha, with a Yes/No confirm
- `/redeploy <app>` — redeploy the current sha, with a Yes/No confirm
- `/help` — the command list

The URL column (WB2) shows the **current working URL**: nginx-proxied apps get a
stable `http://<host>/<path>/`; the tunnel app's URL is read live from the
cloudflared log on every request, so it is always the current quick-tunnel URL
even after the tunnel restarts.

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
