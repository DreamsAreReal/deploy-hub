# ARCHITECTURE — how deploy-hub works and why

Start here if you are new to this repo. This document explains what deploy-hub
is, where each piece of code runs, how one deploy travels from `git push` to a
healthy container, why it is built this way and what tends to bite. It does
not repeat the reference material: every command, metric, config key and
check threshold is in [README.md](README.md); what to do when production is
on fire is in [RUNBOOK.md](RUNBOOK.md).

Reading order for a first day: this file top to bottom, then
[README.md](README.md) sections "How a deploy flows" and "The quality gate",
then run `make test` and read `e2e/scenarios_test.go`.

## 1. What it is, in one paragraph

deploy-hub is a self-hosted continuous delivery system for a handful of small
apps (web services, Telegram bots, static sites) that all live on **one small
VPS** (1 CPU, under 1 GiB RAM). A push to an app repository triggers a GitHub
Actions workflow that builds the Docker image and runs a strict **quality
gate** (tests, coverage, mutation testing, linters, secret and vulnerability
scanners, DAST, load test). Only a commit that passes every check is pushed to
the registry and deployed. On the VPS, a single Go binary started over SSH
verifies who is asking, pulls the exact image that passed the gate, swaps it in
with zero downtime when it can, checks that it is healthy and rolls back when
it is not. Every step is journaled and turned into metrics and alerts. There is
no PaaS, no Kubernetes and no long-running daemon of our own.

## 2. Mental model: one binary, three places

Everything is one Go binary, `deploy-hub` (`cmd/deploy-hub/main.go`). Which
subcommand runs depends on where you are. The subcommand table is
`handlers` in `internal/cli/cli.go`; each entry also lists the only external
programs that subcommand may execute.

| Where | Who runs it | Subcommands | What it does |
|---|---|---|---|
| GitHub Actions runner of an **app** repo | `.github/workflows/deploy.yml` (reusable workflow from this repo) | `tools`, `render`, `gate` | install pinned scanners, render the app's compose file, run the quality gate |
| EU runner `eu-deploy-hub` of **this** repo | `.github/workflows/ci.yml` → `deploy.yml` | `tools`, `gate` | the same gate, run on deploy-hub itself |
| **VPS**, user `deploy` | SSH forced command | `runner` | `deploy`, `report`, `rollback`, `status`, `history` |
| **VPS**, root | sudo (narrow rules), systemd timers, `onboard` over root SSH | `caddy-sync`, `host-sync`, `self-update`, `sudoers-sync`, `prune`, `provision`, `facts` | HTTPS routes, host files from a gated image, image cleanup, preparing `/opt/<app>/` |
| **Operator machine** | you | `onboard` | connect a new app repo end to end |
| **Operator machine**, under an MCP client | Claude Code, Claude Desktop or any MCP client | `mcp` | MCP server on stdio: `status`, `history`, `rollback` tools, each one SSH request to the runner with the deploy key |

```
 app repo (push)                         this repo
     |                                        |
     v                                        v
 caller stub ──uses──> deploy.yml@main ──uses──> actions/hub@<pinned sha>
                           |                     (toolchain + policy/gate.yaml)
                           | docker build, deploy-hub gate
                           | ssh deploy@vps "report ..."   (pass or fail)
                           | docker push (only if gate passed)
                           | ssh deploy@vps "deploy ..."
                           v
 VPS: sshd ── forced command ──> deploy-hub runner
                                   |  verify OIDC, lock, pull by digest
                                   |  blue-green or in-place, health gate
                                   |  journal, metrics, tracker
                                   v
                        docker compose (/opt/<app>)  <── Caddy (HTTPS, sslip.io)
```

## 3. Glossary

| Term | Meaning |
|---|---|
| **gate** | `deploy-hub gate`: about 30 checks that decide if a commit may ship. Code in `internal/gate`, list in `internal/gate/catalog/catalog.go`, thresholds in `policy/gate.yaml`. |
| **verdict** | `verdict.json`, the gate's result: one entry per check with status `pass`/`fail`/`n/a`, values and findings. |
| **runner** | `deploy-hub runner`, the only thing the `deploy` SSH user can execute (`internal/runner`). |
| **forced command** | `command="..."` in `/home/deploy/.ssh/authorized_keys`: whatever the client asks for, sshd runs the runner and passes the request in `SSH_ORIGINAL_COMMAND`. |
| **hub** | this repo plus the server side: `/opt/deploy-hub/` on the VPS. |
| **app** | a deployable repo, listed in `/opt/deploy-hub/apps.list` and in `apps:` of `policy/gate.yaml`. |
| **profile** | `static` or `service` (has a port, HTTP health check), or `bot` (no port, health from the container healthcheck). Stored in `/opt/<app>/app.conf`. |
| **BYOC** | "bring your own compose": the app repo ships a compose file; `render` rewrites it for the server. |
| **standby** | the temporary `<app>-bg` container (`docker compose run` of the main service) used during a blue-green swap; for apps with a port it listens on `port+10000` or a spare port in 30000–32767. |
| **journal** | `/opt/deploy-hub/deploys.log`, one line per operation; the source of truth for history, rollback eligibility and metrics. |
| **state** | `/opt/<app>/.deploy-state` with `current=` and `previous=` tags. |
| **pin** | the commit SHA in `uses: DreamsAreReal/deploy-hub/.github/actions/hub@<sha>` inside `deploy.yml`; decides which gate code and policy app CI runs. |
| **host slug** | the VPS IP with dashes (`203-0-113-7`); apps get `https://<app>.<slug>.sslip.io`. |

## 4. Life of one deploy

Follow this with the code open. Function names are stable; line numbers are
not, so search for the name.

### 4.1 In CI (`.github/workflows/deploy.yml`)

1. The app repo has a tiny caller stub (template in
   `internal/onboard/messages.go`) that calls
   `DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@main` with the app
   name and three secrets (SSH key, VPS host, VPS host key).
2. **Toolchain** step: the composite action `.github/actions/hub/action.yml`,
   pinned by SHA, so GitHub fetches *that* commit of this repo; it builds `deploy-hub`
   and runs `deploy-hub tools`, which downloads every scanner from
   `policy/tools.yaml` and refuses any SHA-256 mismatch (`internal/tools`).
3. **Resolve build inputs**: if the repo has `docker-compose.yml` /
   `docker-compose.yaml` / `compose.yml`, `deploy-hub render` turns it into
   a server-ready compose (`internal/compose`); otherwise the Dockerfile is
   used.
4. **Build** the image locally as `ghcr.io/<owner>/<app>:sha-<commit>`. It is
   not pushed yet.
5. **Quality gate**: `deploy-hub gate ... --image ...` writes `verdict.json`
   and exits non-zero on any failed check (`internal/cli/gate.go`, `runGate`).
6. **Report** (pass or fail; skipped only when the workflow inputs were
   invalid): the verdict is sent
   over SSH as `report <app> sha-<commit>` together with a GitHub OIDC token.
   If a step failed before a verdict existed, the workflow sends a one-check
   verdict named `ci-<step>`, so a broken CI is visible in metrics too.
7. **Push** the image and capture its digest. This step runs only when the
   gate passed.
8. **Deploy**: `ssh deploy@vps "deploy <app> sha-<commit>"`, with `key=value`
   lines on stdin: OIDC token, image digest, an ephemeral `GITHUB_TOKEN` for
   `docker login`, actor, workflow start time and (BYOC) the rendered compose.

### 4.2 On the VPS (`internal/runner`)

1. sshd runs `deploy-hub runner`. `runRunner` in `internal/cli/commands.go`
   reads the request from `SSH_ORIGINAL_COMMAND` and builds a
   `runner.Service` with the real Docker client, health gate, file lock,
   router and OIDC verifier.
2. `Service.Dispatch` (`service.go`) parses the request with `ParseRequest`
   (`request.go`). Only five verbs exist, word counts are capped and the app
   name and tag are validated by regex (`internal/validate`). Anything else is
   refused and journaled as `refuse denied(<fingerprint>)`.
3. The app must be in `apps.list` (`internal/registry/apps.go`); its
   `app.conf` is loaded (`internal/registry/appconf.go`).
4. `ReadMeta` (`meta.go`) parses stdin. `deploy` requires `oidc` and a
   `sha256:` digest.
5. **Identity** (`identity.go`, `verifyIdentity`, and `internal/oidc`): the
   token must be signed by GitHub, carry audience `deploy-hub`, have
   `job_workflow_ref` equal to this repo's `deploy.yml@refs/heads/main`,
   `ref` equal to `refs/heads/main` (a pull request gate run cannot deploy),
   `repository` equal to `repo=` in `app.conf` and `sha` equal to the
   requested commit. GitHub's signing keys are cached on disk, so a GitHub
   outage does not block a deploy.
6. **Lock** (`internal/lock`): one global `flock` on
   `/opt/deploy-hub/deploy.lock`, shared by **all** apps; a second deploy
   waits up to 300 s (`LockWait` in `internal/cli/commands.go`).
7. **Pull by digest** (`deploy.go`, `pull`): `docker login` with the
   one-time token, `docker pull image@digest`, `docker tag` to the sha tag,
   then `docker logout` (deferred, runs even on failure). The running
   container is therefore byte-for-byte the image the gate tested.
8. **Swap** (`bluegreen.go`, `blueGreen`) picks one of three strategies:

   ```
   no running old container ..................... in-place recreate
   label deploy-hub.overlap: "false" on the main
     service (compose wins over the image) ...... in-place recreate
   bluegreen=off in app.conf .................... in-place recreate
   old container has a writable mount ........... in-place recreate (stateful)
     (bluegreen=on overrides this check)
   no port (bots, workers, BYOC without one) .... overlap
   port+10000 unusable and no spare port free
     in 30000-32767 ............................. in-place recreate
   otherwise ..................................... blue-green swap
   ```

   The standby is `<app>-bg`, started with `docker compose run -d --no-deps`
   of the main service (`docker.RunStandby`), so it has the service's command,
   env, volumes, networks, healthcheck and limits, but not its published ports
   or restart policy.

   **Blue-green swap** (`swap`): run `caddy-sync` first so Caddy already
   lists the standby port as a failover upstream. That port is `port+10000`,
   or, when that is past 65535, another app's port or busy, the first free
   port in 30000–32767, just below the Linux ephemeral range
   (`caddy.Routes`, `caddy.StandbyPort`). Spare ports are handed out only
   after every app got its own `port+10000`, so one app's spare never takes
   another app's usual standby port. Start the
   standby on `127.0.0.1:<standby port>` with the new image and health-gate
   it. If it is unhealthy, remove it and stop: production was never touched.
   If healthy, recreate the main service with the new image (Caddy fails over
   to the standby while main is down), gate main, wait `caddy.Settle` (6 s),
   stop the standby gracefully (`docker stop`, its `stop_grace_period`, at
   most 2 minutes) and remove it.

   **Overlap** (`overlap`, no port): the same `handover` without a port and
   without the settle wait. The standby runs next to the old instance and
   must pass the gate (container healthcheck) before main is recreated, so
   both instances run for about 5–90 s.

   **In-place recreate** (`recreate`): `docker compose up` with the new image,
   then gate it. There are a few seconds of 502 for HTTP apps and a gap for
   workers.

   Generated composes, and BYOC services that set none, get
   `stop_grace_period: 30s` (`internal/compose/render.go`), so the old main
   container drains in-flight work on SIGTERM during the recreate.

9. **Health gate** (`internal/health`): poll every 5 s for up to 90 s. HTTP
   apps must answer 2xx/3xx on `127.0.0.1:<port><health_path>`; bots must
   report `healthy` from their own Docker healthcheck; BYOC
   `hmode=process` only needs the container to stay running.
10. **Outcome** (`deploy.go`):
    - healthy: `.deploy-state` advances (`previous` = old `current`), journal
      `deploy ok`, lead time metric;
    - standby unhealthy (blue-green): journal `deploy fail`, the old
      container never stopped, nothing else changes;
    - standby healthy but the recreated main is not: main is started again
      on the old `current` tag (not health-gated), the standby is stopped
      (after 6 s for apps with a port) and removed, journal `deploy fail`;
    - unhealthy after an in-place recreate: redeploy the tag that was
      `current` before this deploy and gate it (journal `rollback`); on a
      first deploy there is none, so the app is stopped.
11. **Always**, deferred in `Dispatch`: push metrics to VictoriaMetrics
    (`internal/obs`) and post delivery steps to the task tracker
    (`internal/delivery`). Both use `context.WithoutCancel`, so they still run
    after SIGTERM, and a failure there never fails the deploy.

For an app with a `host:` spec in the policy, a healthy deploy, rollback or
redeploy ends with `sudo -n deploy-hub host-sync <app> <tag>` (`host.go`,
`syncHost`). The root side (`internal/hostsync`) re-checks that the commit
passed the gate and the image was pulled by digest, installs only the files the
policy lists (owner, mode, exact path), backs up what it replaces, activates
units and Caddy and starts the check units. If anything fails it restores the
previous files and the runner rolls the containers back as for an unhealthy
release (`hostFailed`). Why a separate root command and not more rights for
`deploy`: the policy, not the app repo, decides where root writes, and every
write is tied to a gated digest. Details in README "Host files".

`report` (`report.go`) runs the same identity check, decodes the verdict,
checks it belongs to this app and commit, and records `gate ok|fail` plus
per-check metrics. `rollback` (`rollback.go`) needs no OIDC but only accepts
a tag that this hub already deployed successfully (`previouslyDeployed` in
`identity.go`), so an ungated image cannot be started by hand.

### 4.3 HTTPS routing (`internal/caddy`)

`caddy-sync` renders `/etc/caddy/Caddyfile` from `apps.list`: one vhost per
app with a port, two upstreams (`port` and its standby port, `port+10000` or
a spare one in 30000–32767), `lb_policy first`,
passive health (`max_fails 1`, `fail_duration 5s`) and retries. It validates
the file with `caddy validate`, writes it only when it changed and reloads
Caddy. The runner calls it through `sudo` (`internal/runner/router.go`); the
sudoers rule allows exactly that one command.

## 5. Why it is built this way

| Decision | Why |
|---|---|
| Images are built and tested only in CI, never on the VPS | The VPS has 1 CPU and under 1 GiB RAM; a build or load test there would starve the running apps. |
| SSH with a forced command instead of an HTTP API or agent | No extra daemon or open port. The deploy key can do nothing except call the runner, and the runner accepts five strictly parsed verbs. |
| GitHub OIDC token on every `deploy`/`report` | A leaked SSH key alone is not enough to deploy. The token proves the request comes from this repo's `deploy.yml@main`, for the app's own repo, its `main` branch and this exact commit, so nobody can skip the gate with a workflow of their own. |
| Pull by digest, not by tag | A tag can be moved after the gate ran; the digest cannot. |
| Policy lives only in this repo | App repos cannot lower thresholds or add ignore files (`forbidden_files` in `policy/gate.yaml` bans local scanner configs). Changing the rules is a reviewed commit here. |
| The gate toolchain is pinned by SHA | A gate or policy change reaches app CI only when someone moves the pin on purpose, after it passed CI here. A broken gate commit on `main` cannot block every app at once. |
| Caddy route never changes during a deploy | Caddy keeps the old config alive for HTTP/2 connections after a reload. Switching the route to the standby and back once caused 7 minutes of 502 (#407). Both upstreams are always listed and Caddy fails over passively. |
| Stateful apps are recreated in place | The standby shares the main service's volumes, so an app with SQLite in `/data` would have two processes writing one database during the cutover. A few seconds of 502 are better than corrupted or lost data. |
| Every stateless app overlaps, with or without a port | A worker or bot that is recreated in place stops working until the new one is up. Running the new one next to the old one and gating it first means a broken release never stops the working one; `deploy-hub.overlap: "false"` opts out for singletons. |
| One global deploy lock | Two deploys at once on a 1-CPU box would fight for memory and Caddy reloads. Simplicity beats throughput here. |
| Journal is an append-only text file | Readable with `grep` during an incident without any tool, and enough to rebuild all counters. |
| No comments in code (the `comments` check) | Policy decision of the owner: the reasoning lives in README, this file and commit messages, and names must explain the code. It applies to this repo too. |
| Everything is a subprocess of a fixed binary list (`internal/procexec`) | No shell is ever involved, every executable name is a literal and each subcommand may only run the binaries it declares. It is also the main test seam. |

## 6. Code map

```
cmd/deploy-hub/     main: signal context, calls cli.Main
internal/cli/       subcommand table, config (env + hub.conf), logging, metrics per command
internal/runner/    the SSH forced command: deploy, report, rollback, status, history
internal/oidc/      GitHub OIDC JWT verification, JWKS fetch with disk cache
internal/registry/  apps.list, app.conf, .deploy-state (key=value files)
internal/hub/       paths inside /opt/deploy-hub
internal/journal/   deploys.log: append, parse, query
internal/lock/      global flock
internal/docker/    docker CLI wrapper (pull, run, compose, inspect, ...)
internal/health/    health gate: http, compose healthcheck, process
internal/netprobe/  loopback HTTP probe, "is this port listening"
internal/caddy/     Caddyfile rendering and sync, standby port rule
internal/smoke/     ports from smoke.conf that a deploy must not break
internal/compose/   `render`: repo compose -> server compose (BYOC)
internal/provision/ `provision` and `facts`: prepare /opt/<app>/ as root
internal/hostsync/  `host-sync`: install an app's host files from its gated image as root
internal/selfupdate/ `self-update` and `sudoers-sync`: install a gated release of the hub, write the sudoers rule
internal/onboard/   `onboard` on the operator machine: stub, secrets, provision
internal/mcp/       `mcp`: JSON-RPC over stdio, tool schemas, argument checks before SSH
internal/prune/     keep running image + 2 previous per app
internal/obs/       slog logger, metric names (names.go), pusher, journal-derived counters
internal/delivery/  post deploy steps to the task tracker
internal/tools/     install pinned scanners from policy/tools.yaml
internal/procexec/  allowlisted subprocess runner, memory guard; procexectest = fakes
internal/validate/  regexes for app names, tags, digests, ports, repos
internal/console/   stdout printer
internal/testutil/  assertions for tests
internal/gate/      the quality gate (below)
e2e/                end-to-end tests of the real binary with fake docker/caddy/sudo
policy/             gate.yaml, tools.yaml, golangci.yml, workflow-audit.yml, per-language tool locks
server/systemd/     timers: status every minute, prune daily, history-guard every 5 min
server/vmalert/     alert rules for the hub's metrics
.github/            ci.yml, deploy.yml (reusable), history-guard.yml, actions/hub
```

### The gate (`internal/gate`)

- `gate.go`: the `Check` type, the runner that executes checks in order, the
  verdict format, timeouts, panic recovery (a crashed check fails with
  `crashed=1`, the others still run).
- `internal/gate/catalog/catalog.go`: **the list of checks**, in order. This is where a new
  check is registered.
- `policy/`: loads `policy/gate.yaml` with unknown keys rejected.
- `pack/` and `stacks/`: language packs. A repo is split into projects by
  manifest (`go.mod`, `package.json`, `pyproject.toml`, `*.csproj`, ...);
  each per-language check runs once per project and results are merged
  (worst status wins).
- `golang/`, `python/`, `node/`, `dotnet/`, `shell/`, `static/`,
  `sqlfiles/`: the packs.
- `static/gotmpl/`: renders Go `html/template` sets without data, branch by
  branch, so the static pack validates the markup they produce.
- `source/`: repo-wide text checks: comments, work markers, non-Latin
  letters, forbidden files.
- `mutation/` (Go, AST based) and `tokmut/` (other languages, token based):
  the mutation engines. `lexis/` is the shared lexer, `dupl/` duplication,
  `linecov/` and `junit/` report parsers.
- `scan/`: external scanners (gitleaks, trufflehog, semgrep, osv-scanner,
  syft, zizmor, actionlint, hadolint, trivy).
- `stage/`: starts the built image in an isolated network for DAST (ZAP) and
  the load test.

## 7. How the code is tested

`make test` runs `go test -race -count=1 ./...` and needs no Docker, no SSH and
no network. That works because of three seams:

1. **`procexec.Runner`**: every external program goes through it. Tests use
   `internal/procexec/procexectest` (`Fake`, `Script`) to record calls and
   return canned output.
2. **Interfaces on `runner.Service`**: `Verifier`, `Locker`, `Router`,
   `health.Inspector`/`HTTPChecker`, `obs.Pusher`, `delivery.Notifier`, plus
   injected `Now`, `Sleep` and `Listening` funcs. Docker itself is a
   concrete `docker.Client` over `procexec`, so
   `internal/runner/sim_test.go` plugs an in-memory Docker model in as a
   `procexectest.Fake` handler; most runner tests drive it.
3. **e2e** (`e2e/`): `TestMain` builds the real binary. Each test's hub
   (`newHub` in `e2e/hub_test.go`) symlinks the test binary as `docker`,
   `sudo`, `caddy` and `systemctl` on `PATH`, and it then plays those tools
   (`e2e/fake_test.go`); the fake docker starts real
   HTTP servers. `e2e/issuer_test.go` is a fake GitHub OIDC issuer. A temp
   directory plays `/opt/deploy-hub`. Scenarios are in
   `e2e/scenarios_test.go`; several are registered in `policy/gate.yaml` as
   mandatory scenarios, so renaming them fails the gate.

The full gate on this repo (`make toolchain`, then `make gate`) needs the
tools from `policy/tools.yaml` (linux/amd64) and Docker for Semgrep. In
practice most people push a branch and let CI run it; it takes tens of
minutes, mostly mutation testing.

## 8. Your first day

```
git clone https://github.com/DreamsAreReal/deploy-hub && cd deploy-hub
go version          # must match go.mod
make test           # about 2 minutes, all packages ok
make lint           # golangci-lint with policy/golangci.yml
make build          # linux/amd64 binary in $TMPDIR/deploy-hub-build
```

Then read, in this order: `internal/cli/cli.go` (`Run`, the handler table),
`internal/runner/service.go` (`Dispatch`), `internal/runner/deploy.go`,
`internal/runner/bluegreen.go`, `e2e/scenarios_test.go`
(`TestDeployLifecycle`).

### Rules the gate enforces on every change here

The gate runs on this repo exactly as on apps, so before you push:

- **No comments** in any source or config file: Go, YAML (workflows too),
  Makefile, systemd units, Dockerfiles. In `.go` files every comment token is
  a finding, including `//go:build` and `//go:embed` lines. Explain things in
  names, tests, README, this file or the commit message.
- **No non-Latin letters** in any tracked file, prose included.
- **No work markers**: the words under `markers:` in `policy/gate.yaml`,
  written in uppercase as whole words, are findings in any tracked file.
- **Cognitive complexity at most 9 per function, test functions included.**
  Split helpers early; a long table-driven test is fine, nested branches in a
  test are not.
- **Line coverage, branch coverage and mutation score all at least 90 %.**
  Mutants on lines no test executes count as survivors, so untested code
  lowers the score twice.
- **Duplication below 2 %**, counted over all code including tests.
- **Every test passes with `-race`**, and none is skipped.
- **Dependencies** must be in the approved list in `policy/gate.yaml`, with
  approved licenses.
- **Third-party actions in workflows** are pinned by full commit SHA (zizmor,
  `policy/workflow-audit.yml`); only `DreamsAreReal/deploy-hub/*` may be
  referenced by branch, which is why the stub can say `deploy.yml@main`.

### Common tasks

**Change runner behaviour.** Code in `internal/runner`; add the case to the
simulator tests (`internal/runner/*_test.go`) and, for anything visible over
SSH, a scenario in `e2e/scenarios_test.go`. Update README if the interface or
journal format changes.

**Add a metric.** Declare the name in `internal/obs/names.go`. If it signals a
problem, add it to the list in `TestEveryProblemSignalHasAnAlert`
(`internal/obs/alerts_test.go`) and write an alert in
`server/vmalert/deploy-hub.yml`; the test fails when an alert uses an
undeclared metric or a problem metric has no alert. Document it in README
"Observability".

**Add a gate check.** A repo-wide check is a
`func(context.Context, gate.Workspace) gate.Outcome` added to
`catalog.Checks()`. A per-language check gets a name constant in
`internal/gate/pack/pack.go`, an implementation in every pack in
`internal/gate/stacks/stacks.go` and a kind in `catalog.go`. A new external
tool must be pinned in `policy/tools.yaml` and added to `gateBinaries` in
`internal/cli/cli.go`, otherwise `procexec` refuses to run it. New thresholds
go into the policy struct and its validation.

**Register an app or change policy.** Edit `policy/gate.yaml` (see README
"Registering an app"), merge to `main`, wait for green CI, then **move the
pin** in `.github/workflows/deploy.yml` to that `main` commit in a follow-up
commit. Until the pin moves, app CI keeps the old policy.

**Release the server binary.** Merge to `main`. After the gate passes,
the `deploy.yml` job called by `ci.yml` publishes the release `sha-<commit>`, and within 10 minutes the
server's `deploy-hub-self-update.timer` installs it (README "Releases and
self-update"). Nobody copies binaries to the server; the bootstrap is the only
exception.

## 9. Pitfalls

**Three different versions are live at once.** App CI takes `deploy.yml` from
`main` (the stub says `@main`), but the gate code, scanners and policy from
the **pinned** commit inside it, while the VPS runs whatever binary was last
copied there. A change to workflow steps is live for every app the moment it
lands on `main`; a gate change is live only after the pin moves; a runner
change is live only after a manual install. Check the pin with
`grep 'actions/hub@' .github/workflows/deploy.yml`.

**A deploy refused right after a green gate** (`refuse denied(deploy_...)`,
metric `deploy_hub_identity_verified == 0`, refusal reason
`no-repo-binding` or `identity-unverified`). Causes: `repo=` missing in
`/opt/<app>/app.conf`, the app still deploys with its own workflow instead of
the stub, or the commit does not match. See RUNBOOK "A deploy was rejected".

**An app not in `apps:` of `policy/gate.yaml` is always rejected** by the
`registration` check, and onboarding does not add it there for you.

**Stateful apps get downtime on purpose.** Any writable mount in the live
container means in-place recreate. If the mount is only a cache, set
`bluegreen=on` in `app.conf`. Turning it on for real state means two
instances write it at once during the deploy.

**Bots and workers run twice for a while.** Without a port the new instance
overlaps the old one for about 5–90 s. A Telegram long-poll bot gets
`409 Conflict` in one of them, and a job or update can reach both, so
handlers should be idempotent. Apps that must never run twice (exclusive
lock, singleton consumer) set the label `deploy-hub.overlap: "false"` on the
main service and get an in-place recreate.

**A POST in flight can still get a 502** if the old main container does not
drain on SIGTERM: Caddy retries only GET once a request reached an instance.
Generated composes give it `stop_grace_period: 30s`; apps provisioned before
that default keep Docker's 10 s until the line is added to
`/opt/<app>/docker-compose.yml`, and the app itself must stop accepting and
finish its requests on SIGTERM.

**The standby port moves when `port+10000` is unusable.** Past 65535,
another app's port or listening: the app gets the first free port in
30000–32767, and the route for it is written before the standby starts. Do
not run other services in that range. Only when it is full does the deploy
recreate in place; the CI log shows
`blue-green: no free standby port, falling back to in-place recreate`.

**The lock is global.** A slow health gate of one app (up to 90 s, twice in a
blue-green swap) delays deploys of every other app; over 300 s the waiting
deploy fails. Alert `DeployHubLockContention` fires above 120 s.

**`compose up` errors are only logged.** The health gate decides success, so
read `runner.log` when a deploy fails with a healthy-looking compose file.

**Counters and `increase()`.** VictoriaMetrics' `increase()` counts a series
that appears inside the window as if all its history happened then. Use
`increase_prometheus` for journal counters, as the existing rules do.

**Metrics only exist for apps in `apps.list`.** A removed app's series go
stale; that is intended. Continuous counters depend on
`deploy-hub-status.timer`; if it stops, `DeployHubSilent` fires.

**The gate is slow and memory hungry.** Mutation testing dominates. Each
mutant runs in its own process group with a memory budget and is killed
(counted as detected) when it exceeds it. CI has a 120-minute limit for this
repo, 330 minutes for app deploys.

**History is guarded.** The repo was recreated public on 2026-10-20;
`history-guard.yml` deletes every branch and tag whose root is not the public
snapshot. A clone from before that date cannot push: check
`git merge-base HEAD origin/main` and cherry-pick onto the new `main`.

**Secrets never go in argv or logs.** `onboard` passes secrets to `gh` via
stdin; the runner gets the registry token on stdin and drops it after
`docker login`. Keep it that way in new code.

## 10. Where to look when...

| Question | Look at |
|---|---|
| Why was a commit rejected? | CI log `gate BLOCKED`, artifact `gate-reports/verdict.json`; on the VPS `grep '<app>@' /opt/deploy-hub/deploys.log` |
| What is running right now? | `ssh -i ~/.ssh/deploy_hub_key deploy@<vps> status` |
| What did the runner do? | `/opt/deploy-hub/logs/runner.log` (JSON lines) |
| How do I roll back? | RUNBOOK "Roll back an app" |
| Which metric means what? | README "Observability" |
| What does app.conf contain? | README "Server layout", `internal/registry/appconf.go` |
| Which thresholds apply? | `policy/gate.yaml` |
