# RUNBOOK — when something is on fire

Everything here works without GitHub and without `onboard`. `<app>` is a name
from `/opt/deploy-hub/apps.list` (e.g. `tasks`), `<vps>` is the server address
and `<host-slug>` is the `host_slug=` from `/opt/deploy-hub/hub.conf`. The
deploy key is `~/.ssh/deploy_hub_key` on the operator machine; root access is
`ssh vpn`.

## A deploy was rejected

The CI run of the app shows `gate BLOCKED` with every failed check and its
findings; the same verdict is in the run artifacts (`gate-reports`:
`verdict.json` plus the SBOMs) and on the hub:

    ssh vpn 'grep "<app>@" /opt/deploy-hub/deploys.log | tail -5'     # ... gate fail Ns
    ssh vpn 'grep "gate check failed" /opt/deploy-hub/logs/runner.log | tail -5'

In VictoriaMetrics: `deploy_hub_gate_check_passed{app="<app>"} == 0` lists the
failed checks, `deploy_hub_gate_check_value{app="<app>"}` the measured values.
The fix is always in the app (or, for a policy entry, in this repo's
`policy/gate.yaml` through a reviewed commit) — there is no override.

`refuse denied(deploy_...)` in the journal right after a passing gate means the
runner refused the identity: the app repo deploys with its own workflow instead
of the hub stub, `repo=` is missing from `/opt/<app>/app.conf`, or the commit
does not match. `deploy_hub_identity_verified{app="<app>"} == 0` shows it.

## Roll back an app (bad release live)

    ssh -i ~/.ssh/deploy_hub_key deploy@<vps> "rollback <app>"

Redeploys the previous sha from `/opt/<app>/.deploy-state` (blue-green, so the
current version keeps serving until the target is healthy), waits for the
health gate and journals the result. No pull — the prune policy keeps the
running image plus 2 previous versions on disk.

Redeploy a specific version (may roll FORWARD as well as back): append the full
tag — `"rollback <app> sha-<40-char-sha>"` (it must be one of the kept images;
check with `ssh vpn 'docker images ghcr.io/dreamsarereal/<app>'`) and one this
hub deployed successfully before — the runner refuses any other version. An
explicit-tag operation is journaled as `redeploy` and ends with
`redeployed to requested sha (<sha7>)`; a plain `rollback <app>` is journaled
as `rollback` and ends with `rolled back to previous sha (<sha7>)`.

Before rolling back, see where you would land: `"history <app>"` shows the
recent journal; the exact target is `previous=` in `/opt/<app>/.deploy-state`.

Verify: the command's last line is printed only after the health gate passed.
Then `"status"` must show that sha7 with `running/healthy` (or `running/ok`).
If the target is unhealthy, the command exits non-zero with
`error: rollback target is not healthy` and the current version stays live —
pick an older tag by hand.

## See what is going on

    ssh -i ~/.ssh/deploy_hub_key deploy@<vps> "status"
    ssh vpn 'tail -20 /opt/deploy-hub/deploys.log'          # full journal
    ssh vpn 'tail -50 /opt/deploy-hub/logs/runner.log'      # structured runner logs
    ssh vpn 'docker logs <container> --tail 50'             # app logs
    ssh vpn 'cat /opt/deploy-hub/last-error.log'            # recent pull errors

Journal format: `[ISO] app@sha7 action result Ns [smoke=ok|skip|fail]`
(`smoke=skip` by default: no post-deploy port check is configured — list host
TCP ports in `/opt/deploy-hub/smoke.conf` to enable one).

In VictoriaMetrics:
`deploy_hub_operations_total{result="fail"}`, `deploy_hub_lead_time_seconds`,
`deploy_hub_app_healthy` (refreshed every minute by `deploy-hub-status.timer`;
check it with `ssh vpn 'systemctl list-timers deploy-hub-status.timer'`),
`deploy_hub_gate_passed`. `deploy_hub_lead_time_seconds` has one point per
successful deploy: query it with `last_over_time(deploy_hub_lead_time_seconds[7d])`,
not as an instant value.

A command's final line is a **snapshot of that moment**. The deploy lock
serializes operations so two never race, but a CI push landing while you roll
back by hand can leave the app on a version you did not expect. When in doubt,
trust `status` / `history <app>`, not the last line your terminal printed.

## Confirm a deploy did not break anything

A broken release is discarded on the standby and never reaches the live
container. Apps with a writable data mount (tasks, codeapp, router-bot) skip the
standby and are recreated in place: a broken release there is rolled back to the
previous sha after the gate fails. Check the apps from the operator machine (external is the real
test):

    for a in tasks codeapp; do
      curl -s -o /dev/null -w "$a %{http_code}\n" "https://$a.<host-slug>.sslip.io/"
    done

## A deploy hangs or waits for the lock

Another operation holds `/opt/deploy-hub/deploy.lock`; a waiter prints
`waiting for deploy lock` and gives up after 300 s. Find the holder:

    ssh vpn 'fuser -v /opt/deploy-hub/deploy.lock; ps aux | grep "deploy-hub runner"'

If the holder is gone (killed CI job), the lock is released with its process —
just retry. A leftover standby is `<app>-bg`: `ssh vpn 'docker rm -f <app>-bg'`
(the route needs no change: Caddy only uses the standby port while the
permanent one fails).

## The runner itself is broken

The runner is the root-owned binary `/opt/deploy-hub/bin/deploy-hub`; the SSH
entry is the forced command in `/home/deploy/.ssh/authorized_keys`. Rebuild and
reinstall from this repo with `make build` + `scp` (README, Bootstrap step 5).
Manual rollback bypassing SSH (as root on the VPS):

    sudo -u deploy env SSH_ORIGINAL_COMMAND="rollback <app>" /opt/deploy-hub/bin/deploy-hub runner

Last resort — plain compose, no runner at all (only with an image this hub
already deployed; the runner's checks do not apply here):

    cd /opt/<app> && DEPLOY_IMAGE=<image:tag from .deploy-state> docker compose up -d app

(bring-your-own-compose apps: `docker compose up -d` without the service name).

## The self-hosted CI runner is offline or a job hangs in "Waiting for a runner"

Alert `DeployHubCIRunnerOffline` / `DeployHubCIDiskFull`. Deploy meanwhile
from a GitHub-hosted runner:
`gh variable set DEPLOY_HUB_RUNNER -R DreamsAreReal/<app> -b github-hosted`
(delete the variable to switch back).

```
ssh vpn 'systemctl status ci-netns@<app> ci-docker@<app> ci-runner@<app>'
ssh vpn 'journalctl -u ci-runner@<app> -u ci-docker@<app> -n 100'
ssh vpn 'df -h /var/lib/ci; systemctl restart ci-runner@<app>'
gh api repos/DreamsAreReal/<app>/actions/runners --jq '.runners[] | "\(.name) \(.status)"'
```

A runner deleted on GitHub (or offline for 14 days) loops on "not configured":
remove `/var/lib/ci/<app>/runner` and register it again (README, Self-hosted
CI runner).

## Routes / HTTPS

    ssh vpn 'sudo /opt/deploy-hub/bin/deploy-hub caddy-sync'   # regenerate + reload if changed
    ssh vpn 'journalctl -u caddy --since -1h'

Check a certificate issuer:

    echo | openssl s_client -connect <app>.<host-slug>.sslip.io:443 \
      -servername <app>.<host-slug>.sslip.io 2>/dev/null | openssl x509 -issuer -noout

## Disk / images

Prune runs daily (`deploy-hub-prune.timer`) and keeps running + 2 previous
images per app, counted from `.deploy-state` and the journal, not by image
date. Run it now: `ssh vpn /opt/deploy-hub/bin/deploy-hub prune`.
Check disk: `ssh vpn 'df -h /; docker system df'`.

## Never touch

UFW rules and the `DOCKER-USER` iptables barrier (it drops external→docker
traffic). Caddy owns `:80`/`:443` and its config is generated by
`deploy-hub caddy-sync` from `apps.list` — do not hand-edit
`/etc/caddy/Caddyfile`. The codeapp container IS managed by the runner —
`"rollback codeapp"` is a normal operation.
