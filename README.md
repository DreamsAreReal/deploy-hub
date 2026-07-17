# deploy-hub

Push to the default branch of a connected repo → the app is rebuilt in CI and
running on the VPS a few minutes later, health-checked, with automatic rollback
and a Telegram card. Connecting a new repo is one command: `bin/onboard.sh`.
No PaaS, no daemons: one reusable GitHub Actions workflow, one server-side
script, SSH in between. When things break, see [RUNBOOK.md](RUNBOOK.md).

## How a deploy flows

```
push to default branch
  → caller stub in the app repo (~8 lines)
    → reusable workflow (this repo, .github/workflows/deploy.yml):
        docker build → push ghcr.io/<owner>/<app>:sha-<commit>
        → ssh deploy@vps "deploy <app> sha-<commit>"   (metadata on stdin)
          → runner (/opt/deploy-hub/bin/runner.sh on the VPS):
              flock → docker login (ephemeral job token) → pull → logout
              → compose up → health gate (90s)
              → ok:   journal (incl. VPN smoke) + ✅ Telegram card
              → fail: rollback to previous sha → ⏪ card
                      (first deploy: stop app → ❌ card)
```

Images are built only in CI — never on the VPS (1 CPU, <1 GiB RAM, and a
production VPN that must not be starved).

## Connecting a repo — one command

The repo needs a `Dockerfile` (the app model is one built container per
repo; static service dependencies may sit in the same compose). Then:

```
./bin/onboard.sh <repo> --profile <static|bot|service> [--port N] [--dry-run]
```

It prints the plan, then does everything itself: commits the ~8-line caller
stub to the default branch (opens a PR when the branch is protected), sets
the `VPS_SSH_KEY` repo secret, prepares `/opt/<app>/` on the VPS over your
root SSH access (compose from the template, `app.conf`, empty `.env`), and
adds the app to the allowlist. Idempotent: a second run reports "no changes".

The only thing ever left for you: paste the app's own secrets into
`/opt/<app>/.env` — onboard prints the variable names it finds in the repo.
Then push and watch the deploy.

Prerequisites on the operator machine (one-time, already true here):
`gh` CLI authenticated; root SSH alias `vpn` for the VPS; the deploy key at
`~/.ssh/deploy_hub_key` — one key for all repos, generated at hub setup, its
public half is bound to the forced command on the VPS. Optional
`bin/.env-hub` with `TG_TOKEN=`/`TG_CHAT_ID=` lets onboard set the Telegram
secrets too.

Manual path (no onboard.sh, e.g. rebuilding the hub itself) — see
[RUNBOOK.md](RUNBOOK.md) and `server/app.conf.example`: the pieces are the
caller stub (template in the header of `.github/workflows/deploy.yml`), the
repo secret, `/opt/<app>/` from `server/compose-template.yml`, and a line in
`apps.list`.

## Server layout

| Path | What |
|---|---|
| `/opt/deploy-hub/bin/runner.sh` | the deploy runner — single SSH entry point (forced command) |
| `/opt/deploy-hub/apps.list` | allowlist: `<app> <dir>` per line; unknown apps are refused |
| `/opt/deploy-hub/deploys.log` | append-only journal: `[ISO] app@sha7 action result duration vpn=ok\|fail` |
| `/opt/deploy-hub/telegram.env` | `TG_TOKEN`/`TG_CHAT_ID` for cards (600); absent → cards skipped |
| `/opt/deploy-hub/last-error.log` | recent pull errors (when CI logs are unreachable) |
| `/opt/deploy-hub/bin/prune.sh` | daily image prune (systemd timer): keeps running + 2 previous per app |
| `/opt/<app>/docker-compose.yml` | app container: 127.0.0.1 ports, mem_limit, log rotation |
| `/opt/<app>/app.conf` | `profile=`, `port=`, `health_path=`, `image=` — read by the runner |
| `/opt/<app>/.env` | app secrets (600), referenced by compose, never in git |
| `/opt/<app>/.deploy-state` | `current=`/`previous=` sha tags — rollback source of truth |

## Runner interface

The `deploy` SSH user is locked to `runner.sh` via forced command (no pty, no
forwarding). Accepted requests, everything else is refused and journaled:

```
deploy <app> <tag>      # tag must match ^sha-[0-9a-f]{7,40}$
rollback <app> [tag]    # defaults to the previous sha from .deploy-state
status                  # app | sha | health | last deploy
history <app>           # last journal lines of the app
```

`./bin/onboard.sh status` and `./bin/onboard.sh history <app>` proxy the
same queries from the operator machine.

`deploy` reads `key=value` lines on stdin: `token` (ephemeral GITHUB_TOKEN,
used for `docker login` and dropped — no registry credentials live on the
VPS), `actor`, `subject` (commit line for the card), `start` (workflow start
epoch, for the full push→healthy duration).

## Health gate and rollback

Profile comes from `app.conf`:

- `static`/`service` — `curl` on `127.0.0.1:<port><health_path>` from the VPS,
  up to 90 s;
- `bot` — the container's own healthcheck (functional: Telegram `getMe` with
  the app token) must report `healthy`. A ready-made `getMe` block ships
  commented out in `server/compose-template.yml`: put `BOT_TOKEN` into
  `/opt/<app>/.env`, uncomment the block, make sure the image has wget/curl.

Gate failed → redeploy the previous sha and send a ⏪ card. No previous sha
(first deploy) → stop the app, send a ❌ card; other apps are not touched.

## Telegram card

One render function in `runner.sh` (`render_card`), three lines: status,
`sha7 • commit subject • duration`, and a system pulse (`Deploy #N • M days
stable`, counted from the journal) — or the next action on failure. Cards are
sent by the runner; delivery is confirmed by the Bot API response
(`ok:true` + `message_id`, logged to `telegram.log`).

## Security model

- Separate `deploy` user; its key is bound to the forced command — a leaked
  key yields "deploy an allowlisted app", not a shell.
- App names: strict allowlist; tags: strict regex; anything else refused.
- New containers bind to 127.0.0.1 only (docker bypasses UFW on 0.0.0.0 —
  verified on this VPS) and carry mem limits and log rotation.
- No long-lived registry credentials on the server (ephemeral job token).
- App secrets live in `/opt/<app>/.env` (600), never in git.
