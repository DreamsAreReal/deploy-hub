# deploy-hub

Push to the default branch of a connected repo → the app is rebuilt in CI and
running on the VPS a few minutes later, health-checked, with automatic rollback
and a Telegram card. Once the hub is set up, connecting a new repo is one
command (`bin/onboard.sh`) and under ten minutes. No PaaS, no daemons: one
reusable GitHub Actions workflow, one server-side script, SSH in between.
When things break, see [RUNBOOK.md](RUNBOOK.md).

The VPS is `192.3.94.42`. Two ways in: the `deploy` user (locked to the
runner — how CI and `onboard.sh status` reach it) and your own root access as
SSH alias `vpn` (how you edit `/opt/<app>/.env` and read logs).

**First time on a fresh host?** Do the one-time [Bootstrap](#bootstrap-one-time-host-setup)
below first — until it is done, `onboard.sh` has nothing to talk to.

## How a deploy flows

```
push to default branch
  → caller stub in the app repo (~8 lines)
    → reusable workflow (this repo, .github/workflows/deploy.yml):
        docker build → push ghcr.io/<owner>/<app>:sha-<commit>
        → ssh deploy@vps "deploy <app> sha-<commit>"   (metadata on stdin)
          → runner (/opt/deploy-hub/bin/runner.sh on the VPS):
              flock → docker login (ephemeral job token) → pull → logout
              → blue-green: start a standby, health gate (90s), cut over
              → ok:   journal + ✅ Telegram card (zero-downtime; old never stopped)
              → fail: discard the standby, the current version keeps serving → ⏪ card
                      (first deploy: stop app → ❌ card)
```

Images are built only in CI — never on the VPS (1 CPU, <1 GiB RAM: a build there
would starve the running apps).

## Bootstrap (one-time host setup)

Do this ONCE per hub, before any onboarding. `onboard.sh` and every deploy
assume it is done; skip it and nothing works. It runs from the operator
machine (your laptop) against a VPS you already have root SSH to.

0. **Docker + docker compose on the VPS** — the whole system runs on them.
   On a bare host install first: `ssh vpn 'curl -fsSL https://get.docker.com | sh'`
   (skip if `ssh vpn 'docker compose version'` already prints a version).
1. **gh CLI** authenticated for your GitHub account: `gh auth status`.
2. **Root SSH alias `vpn`** for the VPS in `~/.ssh/config`, e.g.

   ```
   Host vpn
       HostName 192.3.94.42
       User root
   ```

   Check: `ssh vpn 'echo ok'`.
3. **Deploy key** — one ed25519 key for all repos, kept on the operator
   machine, its public half bound to the forced command on the VPS:

   ```
   ssh-keygen -t ed25519 -f ~/.ssh/deploy_hub_key -N '' -C deploy-hub
   PUB=$(cat ~/.ssh/deploy_hub_key.pub)
   ssh vpn bash -s <<EOF
   id deploy >/dev/null 2>&1 || useradd -m -s /bin/bash -G docker deploy
   install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
   printf 'command="/opt/deploy-hub/bin/runner.sh",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding %s\n' '$PUB' \
     > /home/deploy/.ssh/authorized_keys
   chown deploy:deploy /home/deploy/.ssh/authorized_keys
   chmod 600 /home/deploy/.ssh/authorized_keys
   EOF
   ```

4. **Hub directory** on the VPS and the runner scripts from this repo:

   ```
   ssh vpn 'install -d -m 755 /opt/deploy-hub/bin
            : > /opt/deploy-hub/apps.list
            touch /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock
            chown deploy:deploy /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock'
   scp server/runner.sh server/prune.sh vpn:/opt/deploy-hub/bin/
   ssh vpn 'chmod 755 /opt/deploy-hub/bin/*.sh; chown root:root /opt/deploy-hub/bin/*.sh'
   scp server/systemd/deploy-hub-prune.* vpn:/etc/systemd/system/
   ssh vpn 'systemctl daemon-reload && systemctl enable --now deploy-hub-prune.timer'
   ```

5. **Telegram cards (optional)** — without this, deploys still work and the
   cards are only written to `telegram.log`; delivery is off:

   ```
   ssh vpn 'umask 077; printf "TG_TOKEN=...\nTG_CHAT_ID=...\n" > /opt/deploy-hub/telegram.env
            chown deploy:deploy /opt/deploy-hub/telegram.env'
   ```

   To also have `onboard.sh` set the `TG_TOKEN`/`TG_CHAT_ID` repo secrets,
   put the same values in `bin/.env-hub` on the operator machine (gitignored).

Verify the hub answers: `./bin/onboard.sh status` should print a header (an
empty app list is fine on a fresh hub).

## Connecting a repo — one command

Clone this repo and run `onboard.sh` **from its root** (the `./bin/` path is
relative to the repo). The app repo you are connecting needs a `Dockerfile`
(one built image per repo).

### Bring your own compose (recommended)

If the repo ships a `docker-compose.yml`, deploy-hub deploys **that compose**,
so you declare ports / health / env / volumes / sidecars yourself — no "type"
to choose:

- the **main service** (the one with `build:`, or labelled
  `deploy-hub.main: "true"`, or the only service) is the image CI builds and
  pushes to GHCR; its `build:` is swapped for that image (the server never
  builds). Sidecars (postgres/redis/…) are pulled as-is.
- the main service's published port gets the HTTPS route
  (`https://<app>.192-3-94-42.sslip.io`); `deploy-hub.public: "false"` opts out
  of a URL, and `deploy-hub.host: "true"` picks which service's port to expose.
- a `healthcheck:` in your compose drives the health gate; otherwise HTTP on the
  published port, otherwise a process-up check.
- safe defaults (`mem_limit`, json-file log rotation, `restart: unless-stopped`)
  are layered on **only where you did not set them**.

Repos with just a `Dockerfile` (no compose) use the auto-detected template path
below.

```
git clone https://github.com/DreamsAreReal/deploy-hub && cd deploy-hub
./bin/onboard.sh <repo> --profile <static|bot|service> [--port N] [--dry-run]
```

`--dry-run` prints the exact plan (stub commit, secret, `/opt/<app>/` files,
allowlist line) and changes nothing — run it first to see what will happen.
`--port` is optional: for `static`/`service` onboard picks the first free
`127.0.0.1` port from 9001 up (override with `--port N`); `bot` has no port.

Without `--dry-run` it does everything itself: commits the ~8-line caller
stub to the default branch (opens a PR when the branch is protected), sets
the `VPS_SSH_KEY` repo secret, prepares `/opt/<app>/` on the VPS over your
root SSH access (compose from the template, `app.conf`, empty `.env`), and
adds the app to the allowlist. Idempotent: a second run reports "no changes".

**What is left for you afterwards:**

- **static site with no secrets** (the common case): nothing — onboard prints
  `nothing left to do by hand`. Just `git push` to the default branch.
- **app with secrets** (bots, backends): paste them into `/opt/<app>/.env` on
  the VPS before the first push — onboard lists the variable names it found in
  the repo. Edit it as root: `ssh vpn 'nano /opt/<app>/.env'` (mode stays 600).

Then push and watch the deploy: the Actions run on GitHub, the Telegram card
(if configured), or `./bin/onboard.sh status` / `./bin/onboard.sh history <app>`.

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
| `/opt/deploy-hub/deploys.log` | append-only journal: `[ISO] app@sha7 action result duration smoke=ok\|skip\|fail` |
| `/opt/deploy-hub/smoke.conf` | optional: host TCP ports a deploy must not break; absent → `smoke=skip` |
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
rollback <app> [tag]    # no tag: previous sha (journaled as rollback);
                        # explicit tag: journaled as redeploy (may roll forward)
status                  # app | sha | health | last deploy
history <app>           # last journal lines of the app
```

`./bin/onboard.sh status` and `./bin/onboard.sh history <app>` proxy the
same queries from the operator machine.

`status` columns: `sha` is the journal-style sha7 of the running version;
`health` reads `<container state>/<check>`, where `<check>` is the container's
own healthcheck status when it has one, a live gate probe (`ok`/`fail`) for
static/service apps without one, and `-` when nothing can be probed (app
stopped or not deployed yet); `last deploy` is the latest journal operation
(deploy, rollback or stop).

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
