# deploy-hub Telegram control bot

An interactive Telegram surface over `runner.sh`, running on the VPS. It lets the
operator check status and drive rollbacks/redeploys from the chat instead of SSH.

The primary UI is a button-driven inline-keyboard menu; navigation edits the same
message in place (no new message per tap). Human-facing UI text is Russian;
identifiers, logs and code stay English.

## What it does

The UI follows the RS-05 checklists. A `setMyCommands` menu (the "/" quick menu)
is registered at startup; navigation edits one message per level; each screen
carries a breadcrumb title and a Back button; `answerCallbackQuery` fires on
every callback (empty on navigation, a `⏳` toast on long operations); lists
longer than ~6 paginate. Status glyph: `🚀` healthy · `⚠️` running-no-signal ·
`🔴` down.

**Buttons (`/start` or `/menu`):**

- Main menu → apps / status / connect / server / help.
- Apps → apps as buttons (2 per row) with a status glyph; paginated `[◀ i/N ▶]`
  past ~6; back.
- Connect → the owner's GitHub repos (non-archived, non-fork, newest first, with
  pagination; already-onboarded repos are hidden) → tap a repo → the bot
  auto-detects the profile and shows the result to confirm (Connect / Change
  type / Cancel); one tap onboards it end-to-end and replies with the app's HTTPS
  URL. "Change type" opens the manual static/service/bot menu. See "Connect flow
  (WB4)" below.
- App card (RS-05 layout) → status glyph + breadcrumb title (`Приложения › app`),
  health in `<code>`, bullet metrics (sha, deploy #N + time), a clickable URL,
  and a one-line verdict. Actions: logs / refresh, redeploy, and the destructive
  rollback on its own row, then back. Refresh re-reads status; logs go as a
  separate message.
- Server → host RAM (used/total + available), disk (use% + free), uptime,
  running container count.
- Rollback / Redeploy → inline `Да` / `Отмена` confirm; the action shows a
  visible `⏳` loading state, then the card is refreshed with fresh status.

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

## Connect flow (WB4)

**Profile auto-detection.** Tapping a repo detects the profile from its contents
(read-only): an `EXPOSE <port>` in the Dockerfile or a published port in a root
`docker-compose.yml` means a web app (**service** — it gets an HTTPS URL, and the
detected port becomes the container port); a Dockerfile with neither means a
**bot** (no URL, process/functional health). No Dockerfile at all is refused
rather than guessed. `static` is a manual override only (the static/service split
is a cosmetic health-path difference). "Change type" reveals the manual
static/service/bot menu. `onboard.sh` does the same: `--profile` is optional and,
when omitted, the detected profile is printed in the plan before applying.

The bot runs as `deploy` (no root, no `gh`), so onboarding is split cleanly:

- **GitHub side — in the bot** via `github.env` (`GH_TOKEN`, `GH_OWNER`): list
  repos, create `.github/workflows/deploy.yml` on the default branch (contents
  API), and set the `VPS_SSH_KEY` / `TG_TOKEN` / `TG_CHAT_ID` secrets (secrets
  API). Secret values are sealed with libsodium `crypto_box_seal` via `ctypes`
  against the system `libsodium.so` — no pip dependency. The token is only ever
  an `Authorization` header, never argv/env/log.
- **Server side — one privileged helper**, `bin/wb4-provision.sh`, run through a
  narrow sudoers rule (`deploy ALL=(root) NOPASSWD: …/wb4-provision.sh`). It
  creates `/opt/<app>` (compose from the template, `app.conf`, empty `.env`),
  adds the `apps.list` line, and runs `caddy-sync`. Every argument is
  regex-validated inside the helper, so the unprivileged caller has no
  arbitrary-shell surface. This is the only escalation and it does no GitHub work
  and reads no secrets.

The workflow commit is written last, so it triggers the first deploy. The result
card links `https://<app>.192-3-94-42.sslip.io`.

Prerequisites for this flow (place once, mode 600 owner `deploy`):
`/opt/deploy-hub/github.env`, `/opt/deploy-hub/vps_ssh_key` (the private deploy
key, same material already held as the caller repos' `VPS_SSH_KEY` secret), the
`wb4-provision.sh` helper (root-owned) and its sudoers rule, and the
`compose-template.yml` next to the hub.

Security: the whole flow is gated on `chat.id == from.id == TG_CHAT_ID`;
onboarding (which writes to a repo) runs only after the explicit confirm button.
