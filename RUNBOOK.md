# RUNBOOK — when something is on fire

Everything here works without GitHub and without onboard.sh. `<app>` is a
name from `/opt/deploy-hub/apps.list` (e.g. `portfolio`). The deploy key is
`~/.ssh/deploy_hub_key` on the operator machine; root access is `ssh vpn`.

## Roll back an app (bad release live)

    ssh -i ~/.ssh/deploy_hub_key deploy@192.3.94.42 "rollback <app>"

Redeploys the previous sha from `/opt/<app>/.deploy-state`, waits for the
health gate, journals the result. Takes seconds; no pull — the prune policy
keeps the running image plus 2 previous versions on disk.

Roll back to a specific version: append the tag —
`"rollback <app> sha-<full-or-7-char-sha>"` (must be one of the kept images;
check with `ssh vpn 'docker images ghcr.io/dreamsarereal/<app>'`).

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

Xray VPN (ports 2096/8443), nginx:80, the codex container and its
cloudflared tunnel, UFW rules. If a deploy ever kills the VPN
(`vpn=fail` in the journal), fix the VPN before anything else:
`ssh vpn 'cd /opt/xray && docker compose up -d'`.
