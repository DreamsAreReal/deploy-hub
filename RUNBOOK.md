# RUNBOOK — when something is on fire

Everything here works without GitHub and without onboard.sh. `<app>` is a
name from `/opt/deploy-hub/apps.list` (e.g. `portfolio`). The deploy key is
`~/.ssh/deploy_hub_key` on the operator machine; root access is `ssh vpn`.

## Roll back an app (bad release live)

    ssh -i ~/.ssh/deploy_hub_key deploy@192.3.94.42 "rollback <app>"

Redeploys the previous sha from `/opt/<app>/.deploy-state`, waits for the
health gate, journals the result. Takes seconds; no pull — the prune policy
keeps the running image plus 2 previous versions on disk.

Redeploy a specific version (may roll FORWARD as well as back): append the
tag — `"rollback <app> sha-<full-or-7-char-sha>"` (must be one of the kept
images; check with `ssh vpn 'docker images ghcr.io/dreamsarereal/<app>'`).
An explicit-tag operation is journaled as `redeploy` and ends with
`redeployed to requested sha (<sha7>)` + a ✅ card; a plain `rollback <app>`
is journaled as `rollback` with a ⏪ card.

Before rolling back, see where you would land: `"history <app>"` shows the
recent journal (deploys, rollbacks and their sha7); the exact target is
`previous=` in `/opt/<app>/.deploy-state`.

Verify the rollback worked: the command itself must end with
`rolled back to previous sha (<sha7>)` — the health gate has already passed
at that point. Then `"status"` must show that sha7 with `running/healthy`
(or `running/ok`), and a version-stamped page shows the old sha again.
If the target is unhealthy too, the command exits non-zero with
`rollback target is not healthy either` — pick an older tag by hand.

## See what is going on

    ssh -i ~/.ssh/deploy_hub_key deploy@192.3.94.42 "status"
    ssh vpn 'tail -20 /opt/deploy-hub/deploys.log'    # full journal (root)
    ssh vpn 'docker logs <container> --tail 50'       # app logs
    ssh vpn 'cat /opt/deploy-hub/last-error.log'      # recent pull errors

Journal format: `[ISO] app@sha7 action result duration vpn=ok|fail`.
`vpn=fail` on any line → check the VPN first: `nc -z 192.3.94.42 2096`.

## Deploy runner itself is broken

The runner is one root-owned script: `/opt/deploy-hub/bin/runner.sh`
(canonical copy: `server/runner.sh` in this repo — diff against it).
SSH entry = forced command in `/home/deploy/.ssh/authorized_keys`.
Manual rollback bypassing SSH (as root on the VPS):

    sudo -u deploy env SSH_ORIGINAL_COMMAND="rollback <app>" /opt/deploy-hub/bin/runner.sh

Last resort — plain compose, no runner at all:

    cd /opt/<app> && DEPLOY_IMAGE=<image:tag from .deploy-state> docker compose up -d app

## Disk / images

Prune runs daily (systemd `deploy-hub-prune.timer`), keeps running + 2
previous images per app. Run it now: `ssh vpn /opt/deploy-hub/bin/prune.sh`.
Check disk: `ssh vpn 'df -h /; docker system df'`.

## Never touch

Xray VPN (ports 2096/8443), the cloudflared tunnel service itself
(`codex-tunnel.service`) and UFW rules. The codeapp container IS managed by
the runner — `"rollback codeapp"` is a normal operation, not a violation.
nginx is shared ground: the system owns only its location blocks
(`/portfolio/ /zhaba/ /vote/ /api/`); the `/fef…/` location and the base
config are off-limits. If a deploy ever kills the VPN (`vpn=fail` in the
journal), fix the VPN before anything else:
`ssh vpn 'cd /opt/xray && docker compose up -d'`.
