# deploy-hub

Push to the default branch of a connected repo → CI runs the **quality gate**
on the commit and the built image; only a commit that passes every check is
pushed and deployed to the VPS, health-checked, with zero-downtime blue-green
cutover and automatic rollback. A rejected commit never reaches the server: the
verdict of every check lands in the deploy journal, structured logs and
metrics. Connecting a new repo is one command (`deploy-hub onboard`). No PaaS,
no daemons: one reusable GitHub Actions workflow, one Go binary on the server,
SSH in between. When things break, see [RUNBOOK.md](RUNBOOK.md).

The VPS address is configuration, not code: `VPS_ADDR` on the operator machine,
the `VPS_HOST`/`VPS_HOST_KEY` secrets in each app repo and
`/opt/deploy-hub/hub.conf` on the server. Two ways in: the `deploy` user
(locked to the runner — how CI and `onboard status` reach it) and your own root
access as SSH alias `vpn` (how you edit `/opt/<app>/.env` and read logs).

## How a deploy flows

```
push to default branch
  → caller stub in the app repo
    → reusable workflow (this repo, .github/workflows/deploy.yml), one job:
        toolchain: deploy-hub + pinned scanners (sha256-verified)
        [repo has a compose file] deploy-hub render
        docker build (local, not pushed yet)
        deploy-hub gate  → verdict.json + SBOMs (kept as workflow artifacts)
        ssh deploy@vps "report <app> <tag>"  ← always, pass or fail
        gate failed → stop: nothing is pushed or deployed
        docker push → digest
        ssh deploy@vps "deploy <app> <tag>"   (GitHub OIDC token + digest on stdin)
          → deploy-hub runner (forced command on the VPS):
              verify the OIDC token: signed by GitHub, issued to the hub's
              deploy.yml@main, for the app's own repository and this commit
              lock → docker login (ephemeral job token) → pull <image>@<digest> → logout
              → blue-green: start a standby, health gate (90s), recreate the main
                service (Caddy fails over to the standby), gate it, drop the standby
              → ok:   state advanced, journal line, metrics pushed
              → fail: standby discarded, the current version keeps serving
```

Images are built only in CI — never on the VPS (1 CPU, <1 GiB RAM: a build
there would starve the running apps). For the same reason load tests never
touch the VPS (see below).

## The quality gate

`deploy-hub gate` runs every check against the checkout and the locally built
image, writes `verdict.json` and exits non-zero when any check fails. The
deploy workflow runs it for every app, the CI of this repo runs it for
deploy-hub itself. Thresholds, the approved dependencies and licenses, pinned
scanner images and the registry of mandatory scenarios live only in
[`policy/gate.yaml`](policy/gate.yaml) in this repo; an app repo cannot change
them. Lint rules: [`policy/golangci.yml`](policy/golangci.yml); workflow audit
rules: [`policy/workflow-audit.yml`](policy/workflow-audit.yml); scanner
versions and checksums: [`policy/tools.yaml`](policy/tools.yaml).

| Check | What blocks a deploy | Tool |
|---|---|---|
| `registration` | app missing from the policy or deployed from another repository | — |
| `hygiene` | state/backup/debug/report files, keys, scanner-suppression files tracked in git | — |
| `comments` | any comment or documentation string in source and configuration files of any language (Python docstrings included); prose (`.md`, `.txt`, …) and tool-generated lockfiles are not source, `// indirect` is the only note allowed in `go.mod` | go/scanner, YAML parser, chroma lexers |
| `markers` | work markers (the four listed in the policy) anywhere in tracked files | — |
| `stacks` | source files no language pack can check: a language without a pack, or code outside any project | built in |
| `modules` | dependency integrity: unverifiable checksums, lockfile drift | per stack |
| `dependencies` | a dependency (direct, transitive or test-only) not in the approved list | per stack |
| `lint` | any linter finding under the central config (magic numbers/strings, security rules, unused code, …) | per stack |
| `cognitive-complexity` | a function above 9 | per stack |
| `duplication` | 2% or more duplicated code lines across every programming-language file (token windows of 50; imports, includes and comments excluded) | built in, chroma lexers |
| `dead-code` | unreachable functions, including ones reachable only from tests | per stack |
| `tests` | any failed, skipped or crashed test; unit, integration and e2e together | per stack |
| `line-coverage` | below 90% of code lines | per stack |
| `branch-coverage` | below 90% of branches | per stack |
| `scenarios` | a registered security or business scenario whose tests did not pass in any project | policy registry |
| `fuzz` | a fuzz target failing within the time budget, or no fuzz targets at all | per stack |
| `mutation` | mutation score below 90% (killed / all viable mutants, uncovered ones count as survivors), or any mutant that could not be run (the run is incomplete) | per stack |
| `secrets-gitleaks` | any secret in the full git history or the working tree | gitleaks |
| `secrets-trufflehog` | any verified, unverified or unknown secret in the history | trufflehog |
| `sast` | any Semgrep finding of any severity, no ignore files honoured | semgrep |
| `vulnerabilities` | any known vulnerability reachable or imported | per stack |
| `sca` | any known vulnerability in dependencies | osv-scanner |
| `sbom-licenses` | a dependency with an unknown or unapproved license; SBOMs are kept per release | syft |
| `workflows` | any finding in `.github` at the auditor level | zizmor, actionlint |
| `dockerfile` | any hadolint rule, including info level | hadolint |
| `iac` | any misconfiguration in Dockerfiles, compose and other IaC | trivy config |
| `image` | any vulnerability, secret or misconfiguration in the built image | trivy |
| `dast` | any ZAP alert of Low risk or above on the running app | OWASP ZAP full scan |
| `load` | p95 above the profile, errors above the profile, crash, restart or OOM under load | built in |

**Language packs.** The checks marked "per stack" run once per project in the
repository; a project is the directory of a stack manifest. A repository with
several projects (an API and a web front end, say) gets every project checked
and the results merged: the worst status wins, values and findings carry the
project label (`go`, `node:web`). Each project writes its reports to its own
subdirectory of `--out`. Code of a language without a pack, or outside any
project, fails the `stacks` check.

| Stack | Project manifest | Tools |
|---|---|---|
| Go | `go.mod` | go (`mod verify`, `list -deps`, `test -race -cover`, `test -fuzz`), golangci-lint, gocognit, deadcode, gobco, govulncheck, the built-in mutation engine (isolated build cache in the report directory) |
| Python | `pyproject.toml`, `requirements*.txt`; loose `.py` files form a root project | uv, ruff, mypy `--strict`, complexipy, vulture, pytest with coverage (branches, per-test contexts), hypothesis, trivy, the token mutation engine |
| Node / TypeScript | `package.json` | npm, eslint (typescript-eslint strict, sonarjs), `tsc --strict`, knip, vitest / jest / mocha / `node --test`, fast-check, trivy, the token mutation engine |
| .NET | `*.sln`, `*.slnx`, `*.csproj`, `*.fsproj`, `*.vbproj` (the outermost one) | dotnet (`restore --locked-mode`, build with the recommended analyzers and SonarAnalyzer as errors, `format whitespace`, `test` with coverlet), CsCheck / FsCheck, trivy, the token mutation engine |
| Shell | none; `.sh` and `.bash` files form a root project | shellcheck (every optional check), shfmt, bats with bash xtrace coverage, the token mutation engine |
| Static site | none; `.html`, `.htm` and `.css` files form a root project | html-validate (recommended rules, SRI on cross-origin resources), stylelint standard |

What each stack expects from an app repo:

- **Python.** Dependencies are locked: `pyproject.toml` with `uv.lock`, or
  requirements files where every line is pinned with `--hash`. Tests run with
  pytest from the hub's own lock (an app does not pin pytest, coverage or
  hypothesis); a scenario names a test by its pytest node id
  (`tests/test_auth.py::test_invalid_token`). Fuzz targets are hypothesis
  property tests; the policy `fuzz_examples` sets their budget.
- **Node / TypeScript.** `package-lock.json` next to every `package.json`; the
  test runner is the one the project depends on (vitest, jest, mocha, or
  `node --test` when none is installed). Fuzz targets are fast-check property
  tests. A scenario names a test as `path/to/file.test.ts > test name`.
  Linters, `tsc` and knip come from the hub's lock in `policy/node`.
- **.NET.** `RestorePackagesWithLockFile` with a `packages.lock.json` next to
  every project; test projects reference `coverlet.collector`. Analyzer
  severities, `TreatWarningsAsErrors` and `.editorconfig` are injected
  centrally; `#pragma warning disable`, `SuppressMessage` and
  `ExcludeFromCodeCoverage` are findings. Fuzz targets use CsCheck or FsCheck.
  A scenario names a test by its fully qualified name.
- **Shell.** Scripts are bash; tests are `.bats` files. A scenario names a
  test as `file.bats > test name`. Shell has no package dependencies and no
  property-based fuzzer, so `dependencies`, `modules`, `vulnerabilities` and
  `fuzz` are `n/a` with that reason.
- **Static site.** Markup and styles carry no executable code: complexity,
  dead code, tests, coverage, fuzzing and mutation are `n/a`. Every
  cross-origin script or stylesheet needs an `integrity` hash. JavaScript
  belongs to a Node project with its own `package.json`.

Approved dependencies in the policy use the Go module path for Go and a stack
prefix for the rest: `pypi:requests`, `npm:express`, `nuget:Serilog`. When a
package publishes no machine-readable license, `declared_licenses` in the
policy records it by package URL without a version
(`pkg:npm/some-lib: MIT`); the license must be on the approved list.

**Whose code.** Repositories of the policy `owners` that are not forks get every
check. Forks and other code get only the security checks (secrets, SAST,
dependencies, licenses, workflows, image, DAST); the quality checks are
reported as `n/a`. Checks that need an image or an HTTP surface are `n/a` for
app kinds without one (`binary`, `worker`). Nothing else is ever skipped, and a
check that cannot finish (tool crash, timeout) fails.

**Mutation testing** is a built-in engine with every mandatory operator:
condition inversion, comparison and boundary changes, arithmetic and logical
operator swaps, `++`/`--`, removed negations, changed return values, removed
statements and disabled error handling. Each mutant is compiled with
`go test -overlay` and tested by its own package first, then by every package
that depends on it. Mutants on lines no test executes count as survivors;
mutants that do not compile are not mutants and are not counted. There is no
way to exclude a mutant. Every mutant's test run is its own process group with
a memory budget (the machine's memory divided by the workers plus one); a run
that exceeds it is killed and the mutant counts as detected (`out-of-memory`),
like a timeout, so a mutant that turns a loop into an endless allocation cannot
take the CI runner down. The language packs mutate with the same engine and the
same budget.

**Load test, done for a 1-CPU VPS.** The image runs in an isolated docker
network on the CI runner with the production limits from the policy (`cpus`,
`memory`, no swap, pid limit); a built-in generator sends the app's profile
(`rate`, `duration`, `concurrency`, `paths`) and the check fails on p95 above
the limit, errors above the limit, a crash, a restart or an OOM kill. The VPS
itself is never load-tested.

**DAST.** The same isolated stage runs an OWASP ZAP full scan (or the API scan
when the app declares an OpenAPI path) for the time budget in the policy; any
alert of Low risk or above blocks the deploy.

**Scenario registry.** Each app lists its mandatory security and business
scenarios in the policy, each mapped to the tests that prove it
(`package.TestName`, the package path relative to the module). Deleting or
renaming a test does not remove the requirement — the scenario fails.

**Registering an app** (in this repo, reviewed like any change):

```yaml
apps:
  myapp:
    repo: DreamsAreReal/myapp
    kind: service            # service | worker | binary
    port: 8080               # container port
    health: /health
    cpus: "0.5"              # production limits used by the stage
    memory: 256m
    env: [APP_MODE=test]     # non-secret settings the app needs to boot
    dast: {target: /api, spider: 1m, max_time: 10m}
    load: {rate: 20, duration: 30s, p95: 300ms, max_error_percent: 0, concurrency: 10, paths: [/health]}
    scenarios:
      - id: invalid-token-is-refused
        category: security
        tests: [internal/auth.TestInvalidToken]
```

**Releasing a gate or policy change.** App CI runs the gate code, the toolchain
manifest and the policy of the commit that `.github/workflows/deploy.yml` pins
in `uses: DreamsAreReal/deploy-hub/.github/actions/hub@<sha>` — an immutable
reference, so nothing reaches app CI by accident. After a change to `policy/`,
the gate code or the toolchain lands on `main` (registering an app is such a
change), point that `uses:` line at the new `main` commit in a follow-up commit.

**Policy decisions that look like exceptions.** Two analyzer rules contradict
other requirements and are turned off centrally for every repo:

- gosec G204 (subprocess with variable arguments) cannot be satisfied by any
  tool that runs subprocesses with arguments (the runner drives `docker`, the
  gate drives the scanners). Command injection is covered instead by Semgrep's
  `dangerous-exec-command`/`dangerous-exec-cmd`, which stay on and pass: every
  executable is a literal from a fixed table in `internal/procexec`, no shell is
  ever involved and arguments are validated before use.
- zizmor `undocumented-permissions` demands a comment next to every
  write permission, while comments are banned by policy.

**Toolchains.** `policy/tools.yaml` pins every scanner and language toolchain
(uv, ruff, node and npm, the .NET SDK, shellcheck, shfmt, bats) by URL and
SHA-256; `make toolchain` downloads them, refuses any checksum mismatch and
puts them into one directory for `PATH`. Archives that ship a whole tree (node,
the .NET SDK, bats, shellcheck) are unpacked under `trees.d` with links to their
executables. Bash itself comes from the CI runner (4.1 or newer).

**Run it locally** (Go toolchain plus the tools from `policy/tools.yaml` on
`PATH`; `make toolchain` installs them on linux/amd64):

```
make gate                                   # the gate on deploy-hub itself
go run ./cmd/deploy-hub gate --app myapp --repo Owner/myapp --commit $(git rev-parse HEAD) \
  --policy /path/to/deploy-hub/policy/gate.yaml --src "$PWD" --out /tmp/gate --image myapp:local
```

`--src` and `--out` are absolute paths; reports go to `--out`, which must be
outside the checkout.

## The binary

One statically linked binary, `/opt/deploy-hub/bin/deploy-hub`, with these
commands:

| Command | Runs as | What |
|---|---|---|
| `runner` | `deploy` (SSH forced command) | `deploy`, `report`, `rollback`, `status`, `history` |
| `render <app> <in> <out>` | CI | turns a repo compose into a deploy-ready one |
| `gate …` | CI | the quality gate |
| `tools --manifest … --dir …` | CI | installs the pinned scanners, checksums verified |
| `caddy-sync` | root (via a narrow sudoers rule) | regenerates the Caddyfile from `apps.list` |
| `prune` | root (systemd timer) | keeps the running image + 2 previous per app |
| `provision --app … ` | root (from `onboard`) | prepares `/opt/<app>/` and the allowlist line |
| `facts <app>` | root (from `onboard`) | reports what already exists on the server |
| `onboard <repo> …` | operator machine | connects a GitHub repo end-to-end |

Build it with `make build` (linux/amd64 by default, output outside the repo in
`$TMPDIR/deploy-hub-build/`); `make build GOOS=darwin GOARCH=arm64` gives a
binary for the operator laptop, or use `go run ./cmd/deploy-hub onboard …`.

## Bootstrap (one-time host setup)

Do this once per hub, from the operator machine, against a VPS you already have
root SSH to. `<vps>` is the server address.

0. **Docker + docker compose** on the VPS:
   `ssh vpn 'curl -fsSL https://get.docker.com | sh'` (skip if
   `ssh vpn 'docker compose version'` prints a version).
1. **gh CLI** authenticated for your GitHub account: `gh auth status`.
2. **Root SSH alias `vpn`** in `~/.ssh/config` (`HostName <vps>`, `User root`).
   Check: `ssh vpn 'echo ok'`. Connect once as `deploy@<vps>` so the host key is
   in `~/.ssh/known_hosts` — `onboard` copies it into the repo secrets.
3. **Caddy** installed as the systemd service `caddy`, owning `:80`/`:443`.
4. **Deploy key** — one ed25519 key for all repos, bound to the forced command:

   ```
   ssh-keygen -t ed25519 -f ~/.ssh/deploy_hub_key -N '' -C deploy-hub
   PUB=$(cat ~/.ssh/deploy_hub_key.pub)
   ssh vpn bash -s <<EOF
   id deploy >/dev/null 2>&1 || useradd -m -s /bin/bash -G docker deploy
   install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
   printf 'command="/opt/deploy-hub/bin/deploy-hub runner",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding %s\n' '$PUB' \
     > /home/deploy/.ssh/authorized_keys
   chown deploy:deploy /home/deploy/.ssh/authorized_keys
   chmod 600 /home/deploy/.ssh/authorized_keys
   EOF
   ```

5. **Hub directory, server config and binary** (`<host-slug>` is the address
   with dashes, as used by sslip.io, e.g. `203-0-113-7`):

   ```
   make build
   ssh vpn 'install -d -m 755 /opt/deploy-hub/bin
            install -d -m 750 -o deploy -g deploy /opt/deploy-hub/logs
            printf "host_slug=<host-slug>\n" > /opt/deploy-hub/hub.conf
            touch /opt/deploy-hub/apps.list /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock /opt/deploy-hub/last-error.log
            chown deploy:deploy /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock /opt/deploy-hub/last-error.log'
   scp "${TMPDIR:-/tmp}/deploy-hub-build/deploy-hub" vpn:/opt/deploy-hub/bin/deploy-hub
   ssh vpn 'chown root:root /opt/deploy-hub/bin/deploy-hub && chmod 755 /opt/deploy-hub/bin/deploy-hub'
   ```

   `hub.conf` also accepts `acme_email=` (default `admin@<host-slug>.sslip.io`);
   the environment variables `HOST_SLUG`/`ACME_EMAIL` override it.
   Optional `delivery_url=<tracker>/api/deliveries/<secret>` reports every
   gate verdict, deploy and rollback to the task tracker as delivery steps
   (see [Delivery steps](#delivery-steps)); `DEPLOY_HUB_DELIVERY_URL` overrides it.

6. **sudoers rule** — the runner (user `deploy`) may refresh routes, nothing else:

   ```
   ssh vpn "printf '%s\n' 'deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/deploy-hub caddy-sync' > /etc/sudoers.d/deploy-hub
            chmod 440 /etc/sudoers.d/deploy-hub && visudo -cf /etc/sudoers.d/deploy-hub"
   ```

7. **Daily prune and the per-minute status refresh** (keeps the metrics
   continuous, see Observability):

   ```
   scp server/systemd/deploy-hub-prune.* server/systemd/deploy-hub-status.* vpn:/etc/systemd/system/
   ssh vpn 'systemctl daemon-reload && systemctl enable --now deploy-hub-prune.timer deploy-hub-status.timer'
   ```

8. **history-guard timer** — the `history-guard` workflow deletes every branch
   and tag whose root is not the public snapshot. Its `push` trigger runs the
   workflow file from the pushed commit, so it fires at once only for refs that
   already carry the file; refs built purely on the old history are caught by
   the other triggers. GitHub's `schedule` trigger is best-effort and may
   never fire, so the VPS dispatches the `history-guard` workflow of
   `deploy-hub` and `tasks` every 5 minutes. It needs a fine-grained PAT limited
   to those two repos with only **Actions: Read and write**:

   ```
   printf 'Authorization: Bearer %s\n' '<github_pat_…>' \
     | ssh vpn 'install -d -m 700 /etc/deploy-hub && install -m 600 /dev/stdin /etc/deploy-hub/history-guard.header'
   scp server/systemd/deploy-hub-history-guard.* vpn:/etc/systemd/system/
   ssh vpn 'systemctl daemon-reload && systemctl enable --now deploy-hub-history-guard.timer'
   ```

   Check: `ssh vpn 'systemctl start deploy-hub-history-guard.service'` exits 0
   and `gh run list -R DreamsAreReal/deploy-hub -w history-guard.yml -L 1`
   shows a fresh `workflow_dispatch` run. When the PAT expires, rewrite the
   header file the same way.

9. **Observability** (recommended): the shared logs/metrics stack lives in its
   own repo, [DreamsAreReal/observability](https://github.com/DreamsAreReal/observability).

Verify the hub answers: `VPS_ADDR=<vps> go run ./cmd/deploy-hub onboard status`
prints a header (an empty app list is fine on a fresh hub).

## Connecting a repo — one command

The app repo needs a `Dockerfile` (one built image per repo) and an entry in
`policy/gate.yaml` (see "Registering an app") released to app CI (see
"Releasing a gate or policy change"); without the entry every deploy is
rejected by the `registration` check. Run from a clone of this repo:

```
VPS_ADDR=<vps> go run ./cmd/deploy-hub onboard <repo> [--profile static|bot|service] [--port N] [--dry-run]
```

`--dry-run` prints the exact plan (stub commit, secrets, `/opt/<app>/` files,
allowlist line) and changes nothing — run it first. Without `--profile` the
profile is detected: `EXPOSE` in the Dockerfile or a published port in the repo
compose → `service`, otherwise `bot` (no URL). `--port` is optional: onboard
picks the first free `127.0.0.1` port from 9001 up. Other options: `--cport`,
`--mem`, `--health-path`, `--app`, `--dockerfile`.

Without `--dry-run` it does everything itself: commits the caller stub to the
default branch (opens a PR when the branch is protected), sets the repo secrets
`VPS_SSH_KEY` (from `~/.ssh/deploy_hub_key`), `VPS_HOST` and `VPS_HOST_KEY`
(the `known_hosts` entry of `<vps>`) — always via stdin, never argv — runs
`deploy-hub provision` on the VPS over root SSH (compose, `app.conf` with the
`repo=` binding, empty `.env` 600, allowlist line, Caddy route) and reports
what is left for you. A second run reports "no changes".

Environment for `onboard`: `VPS_ADDR` (required), `VPS_HOST_ALIAS` (root alias,
default `vpn`), `DEPLOY_KEY` (default `~/.ssh/deploy_hub_key`), `REPO_OWNER`
(default `DreamsAreReal`), `HUB_REPO` (default `DreamsAreReal/deploy-hub`).

**Left for you afterwards:** a static site with no secrets — nothing, just push.
An app with secrets — paste them into `/opt/<app>/.env` on the VPS before the
first push (`ssh vpn 'nano /opt/<app>/.env'`, mode stays 600); onboard lists the
variable names it found in `.env.example` or the compose file.

The caller stub that ends up in the app repo:

```yaml
name: deploy
on:
  push: {branches: [main]}
permissions: {}
concurrency: {group: deploy, cancel-in-progress: false}
jobs:
  deploy:
    name: deploy
    permissions: {contents: read, packages: write, id-token: write}
    uses: DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@main
    with: {app: myapp}
    secrets: {VPS_SSH_KEY: "${{ secrets.VPS_SSH_KEY }}", VPS_HOST: "${{ secrets.VPS_HOST }}", VPS_HOST_KEY: "${{ secrets.VPS_HOST_KEY }}"}
```

Reusable workflows and actions of a private repo are shared only with other
private repos of the same owner (repo settings → Actions → Access), so an app
repo that uses the hub must be private while the hub is private.

### Bring your own compose

If the repo ships `docker-compose.yml` (or `.yaml`, or `compose.yml`), CI runs
`deploy-hub render` and the VPS runs **that** compose:

- the **main service** (labelled `deploy-hub.main: "true"`, else the one with
  `build:`, else the only service) is the image CI builds; its `build:` becomes
  `image: ${DEPLOY_IMAGE}` and `container_name: <app>`. Sidecars are pulled
  as-is. CI builds the main service's own `build.context`/`build.dockerfile`.
- every published port of every service is bound to `127.0.0.1`
  (`"8080:80"` becomes `"127.0.0.1:8080:80"`, long syntax gets
  `host_ip: 127.0.0.1`); any other host IP and `network_mode: host` are
  rejected.
- the main service's published port gets the HTTPS route;
  `deploy-hub.public: "false"` opts out, `deploy-hub.host: "true"` picks another
  service's port.
- a `healthcheck:` drives the health gate; otherwise HTTP on the published
  port, otherwise "the container keeps running".
- `mem_limit: 256m`, json-file log rotation and `restart: unless-stopped` are
  added only where you did not set them; `.env` is attached to the main service.

## Server layout

| Path | What |
|---|---|
| `/opt/deploy-hub/bin/deploy-hub` | the binary (root-owned, 755) |
| `/opt/deploy-hub/hub.conf` | `host_slug=`, optional `acme_email=`, `delivery_url=` |
| `/opt/deploy-hub/apps.list` | allowlist: `<app> [dir]` per line; unknown apps are refused |
| `/opt/deploy-hub/deploys.log` | append-only journal: `[ISO] app@sha7 action result Ns [smoke=ok\|skip\|fail]` |
| `/opt/deploy-hub/logs/<command>.log` | structured JSON logs (rotated at 10 MiB) |
| `/opt/deploy-hub/smoke.conf` | optional host TCP ports a deploy must not break; absent → `smoke=skip` |
| `/opt/deploy-hub/last-error.log` | recent pull errors (when CI logs are unreachable) |
| `/home/deploy/.cache/deploy-hub/jwks.json` | last GitHub OIDC signing keys, used when GitHub is unreachable |
| `/opt/<app>/docker-compose.yml` | app stack: 127.0.0.1 ports, mem limit, log rotation |
| `/opt/<app>/app.conf` | `profile=`, `port=`, `health_path=`, `image=`, `repo=` (BYOC: `mode=byoc`, `main=`, `hmode=`) |
| `/opt/<app>/.env` | app secrets (600), referenced by compose, never in git |
| `/opt/<app>/.deploy-state` | `current=`/`previous=` sha tags — rollback source of truth |

## Runner interface

The `deploy` SSH user is locked to `deploy-hub runner` by the forced command.
Accepted requests — anything else is refused and journaled as `refuse`:

```
deploy <app> <tag>      # tag must match ^sha-[0-9a-f]{7,40}$
report <app> <tag>      # gate verdict for that commit
rollback <app> [tag]    # no tag: previous sha (journaled as rollback);
                        # explicit tag: journaled as redeploy (may roll forward)
status                  # app | sha | health | last deploy
history <app>           # last 20 journal lines of the app
```

`deploy` and `report` read `key=value` lines on stdin. Both require `oidc` — a
GitHub Actions OIDC token (audience `deploy-hub`) whose signature verifies
against GitHub's published keys, whose `job_workflow_ref` is
`<HUB_REPO>/.github/workflows/deploy.yml@refs/heads/main`, whose `repository`
is the `repo=` of the app and whose `sha` is the requested commit. A deploy
request from any other workflow, repository or commit is refused. `deploy`
also requires `digest` (`sha256:…` of the pushed, gated image) and accepts
`token` (ephemeral GITHUB_TOKEN, used once for `docker login` and dropped —
never logged or journaled), `actor`, `subject`, `start` (workflow start epoch,
for the push→healthy lead time) and, for bring-your-own-compose, `compose`
(base64), `main`, `hport`, `hmode`. The image is pulled by digest and tagged
locally, so the running container is exactly the gated artifact. `report`
requires `verdict` (base64 of `verdict.json`); it is journaled as `gate ok|fail`
and every check becomes log lines and metrics.

`rollback`/`redeploy` accept only versions this hub deployed successfully
before (current, previous or a successful deploy in the journal), so an image
that never passed the gate cannot be started by hand through the runner.

Runner settings: `DEPLOY_HUB_OIDC_AUDIENCE` (default `deploy-hub`),
`DEPLOY_HUB_JWKS_URL` (default GitHub's), `DEPLOY_HUB_JWKS_CACHE`, `HUB_REPO`.

`status` columns: `sha` is the sha7 of the running version; `health` is
`<container state>/<check>` where `<check>` is the container healthcheck, a live
HTTP probe (`ok`/`fail`) for HTTP apps without one, or `-`.

## Health gate and rollback

The gate polls every 5 s for up to 90 s:

- `static`/`service` (or BYOC `hmode=http`) — HTTP 2xx/3xx from
  `127.0.0.1:<port><health_path>`;
- `bot` (or BYOC `hmode=compose`) — the container's own healthcheck reports
  `healthy`; make it check real work, not just "process is up";
- BYOC `hmode=process` — the main container is still running after one tick.

Blue-green: when the app already runs and has a port, the new image starts as a
standby `<app>-bg` on the fixed loopback port `port+10000` and is gated there.
The real service is then recreated and gated; while it is down Caddy sends its
requests to the standby by itself (see HTTPS routing), so the cutover needs no
Caddy reload. The standby stays up 6 s after the real service is healthy again,
until Caddy is back on the permanent port, and is removed on every exit path,
including SIGINT/SIGTERM/SIGHUP. A broken release never touches the live
container. The deploy recreates in place instead when the app has no standby
port: `port+10000` is past 65535, is another app's port, or something already
listens on it. When the old instance was not running, a failed gate redeploys
the previous sha; on a first deploy the app is stopped.

## Observability

Logs — every server command writes JSON lines to
`/opt/deploy-hub/logs/<command>.log`; `gate` and `tools` log JSON to stderr in
CI (each check, its values and findings; mutation progress). To ship the server
logs to VictoriaLogs, the observability stack's Vector needs a `file` source
over `/opt/deploy-hub/logs/*.log`. Secrets (registry token, OIDC token, deploy
key, `.env` content) are never logged.

### Delivery steps

With `delivery_url=` set, every command also posts what it proved to the task
tracker as a JSON array of steps `{repo, target, sha, step, state}`
(`repo` = `github.com/<app repo>`, `target` = `prod`, `sha` = the journal sha7):

| Journal entry | Steps |
|---|---|
| `gate ok` / `gate fail` | `checks ok` / `checks fail` |
| `deploy ok`, `redeploy ok` | `deploy ok`, `live ok` |
| `deploy fail` | `deploy fail` |
| `rollback ok` | `live ok` for the restored sha |

A failed post is logged and never fails the deploy. The tracker matches the sha
to the tasks whose commits it contains and draws their path to prod.

Metrics — pushed to VictoriaMetrics after every command
(`DEPLOY_HUB_METRICS_URL`, default
`http://127.0.0.1:8428/api/v1/import/prometheus`; set it empty to disable).
`deploy-hub-status.timer` runs `runner status` every minute, so the journal
counters and the health gauges have a sample every minute and `increase()` over
a window counts what happened in it. The counters cover only the apps in
`apps.list` (plus refusals): the journal history of a removed app stops being
pushed and its series go stale. Change failure rate, deploy frequency and lead
time quantiles are recording rules in the observability repo
(`rules/dora.yml`, queries in its README).

| Metric | Labels | Meaning |
|---|---|---|
| `deploy_hub_operations_total` | app, action, result | journal-derived counters (deploy, gate, rollback, redeploy, stop, prune, refuse) |
| `deploy_hub_last_operation_timestamp_seconds` | app, action, result | when each outcome last happened |
| `deploy_hub_last_incident_timestamp_seconds` | app | last failed deploy or rollback — "days stable" = `(time() - this) / 86400` |
| `deploy_hub_operation_duration_seconds` | app, action, result | duration of the operation just finished |
| `deploy_hub_lead_time_seconds` | app | push → healthy, from the workflow start; one sample per successful deploy |
| `deploy_hub_health_gate_duration_seconds` | app, target, result | health gate time for the standby and the main service |
| `deploy_hub_lock_wait_seconds` | app | time spent waiting for another deploy |
| `deploy_hub_image_pull_duration_seconds` | app, result | pull by digest and local tag |
| `deploy_hub_identity_verified` | app, verb | 1 when the OIDC identity of a deploy/report verified, 0 when refused |
| `deploy_hub_gate_passed` | app, own | 1 when the last gate verdict passed |
| `deploy_hub_gate_duration_seconds` | app | wall time of the last gate run |
| `deploy_hub_gate_failed_checks` | app | number of failed checks in the last verdict |
| `deploy_hub_gate_check_passed` | app, check | 1/0 per applicable check |
| `deploy_hub_gate_check_duration_seconds` | app, check | time of each check |
| `deploy_hub_gate_check_value` | app, check, name | every measured value: coverage, mutation score, duplication, findings, p95, … |
| `deploy_hub_app_up`, `deploy_hub_app_healthy` | app | from `status` (every minute) |
| `deploy_hub_apps_registered` | — | apps in the allowlist |
| `deploy_hub_prune_removed_images`, `deploy_hub_prune_kept_images` | app | last prune |
| `deploy_hub_caddy_routes`, `deploy_hub_caddy_sync_success`, `deploy_hub_caddy_reload` | — | last route sync |
| `deploy_hub_provision_created_resources` | app | files/lines created by the last provision |

## HTTPS routing

Every app with a port gets `https://<app>.<host-slug>.sslip.io` →
`reverse_proxy 127.0.0.1:<port> 127.0.0.1:<port+10000>` with an automatic
Let's Encrypt certificate (sslip.io resolves the name back to the VPS).
`deploy-hub caddy-sync` renders `/etc/caddy/Caddyfile` from `apps.list`,
validates it with `caddy validate`, writes it only when it changed (mode 600,
owner `caddy`) and then reloads Caddy. It runs after onboarding and after
every successful deploy, once the standby is gone; an unchanged file means
no reload. Settings: `hub.conf`, `CADDYFILE`, `HOST_SLUG`, `ACME_EMAIL`,
`CADDY_USER`.

The second upstream is the blue-green standby port; nothing listens there
outside a deploy. `lb_policy first` keeps every request on the permanent port
while it answers. A request that fails there (`fail_duration 5s`, `max_fails
1`) takes the port out of rotation for 5 s and goes to the standby. The proxy
retries (`lb_try_duration 30s`, `lb_try_interval 250ms`), so while a container
is recreated, requests wait for one of the two instead of getting an
instant 502. A failed connection is always retried; a request that already
reached an instance and broke is retried only for GET. So a POST that is in
flight at the moment the old main container stops can still get a 502.

The route never changes during a deploy, on purpose: on a reload Caddy keeps
the previous server alive, with its previous upstreams, for as long as a
client holds an HTTP/2 connection open. `grace_period` does not end those
connections. Pointing the route at the standby and back once caused 7 minutes
of 502s to a removed standby (#407).

The global block sets `grace_period 10s`, so a reload stops waiting for the
connections of the replaced config after 10 s. It does not close an HTTP/2
connection that is still in use (checked on Caddy 2.11.7), which is why
blue-green never relies on a reload.

Every app vhost has `log`: Caddy writes one JSON access line per request to
journald, and the shared observability stack ships it to VictoriaLogs
(`log_type:access`). The file also imports `/etc/caddy/extra/*.caddy`, the
hand-managed vhosts that are not deploy-hub apps.

## Security model

- Nothing is deployed without a passing gate: the only workflow the runner
  accepts is the hub's `deploy.yml@main`, proven by a GitHub-signed OIDC token
  bound to the app's repository and commit, and the image is pulled by the
  digest that workflow pushed after the gate.
- Separate `deploy` user; its key is bound to the forced command — a leaked key
  yields "deploy an allowlisted app with a valid GitHub identity", not a shell.
  The runner never invokes a shell; it executes only `docker` and
  `sudo -n deploy-hub caddy-sync`, from a fixed table of literal executables.
- App names: strict regex + allowlist; tags and digests: strict regex; BYOC
  metadata and every provision argument are validated on the server again.
- Containers bind to 127.0.0.1 only and carry mem limits and log rotation.
- No long-lived registry credentials on the server (ephemeral job token via
  stdin, logout guaranteed).
- App secrets live in `/opt/<app>/.env` (600), never in git; files written by
  the binary are 600.
- Workflow actions are pinned by commit SHA; the gate toolchain is pinned by
  version and sha256.

## Development

```
make lint       # golangci-lint with the central config
make test       # go test -race
make build      # linux/amd64 binary in $TMPDIR/deploy-hub-build
make toolchain  # deploy-hub + pinned scanners into $TMPDIR/deploy-hub-build/toolchain (linux/amd64)
make gate       # the full quality gate on this repo (reports in $TMPDIR/deploy-hub-build/gate)
```

Requires Go (version in `go.mod`) and, for `make gate`, the tools listed in
`policy/tools.yaml` plus Docker (Semgrep runs in a container). Build output and
reports never land in the checkout. CI (`.github/workflows/ci.yml`) runs the
same gate on every push and pull request.

The deploy workflow takes the gate toolchain — this binary, the policy and the
scanner manifest — from the commit its `uses:` line pins. A change to the gate
or the policy reaches app deploys when that pin in
`.github/workflows/deploy.yml` is moved to a commit that passed CI.

## Migrating a running hub

From the bash version:

1. Install the binary (Bootstrap step 5) and create `/opt/deploy-hub/logs`.
2. Point the forced command at it: in `/home/deploy/.ssh/authorized_keys`
   replace `command="/opt/deploy-hub/bin/runner.sh"` with
   `command="/opt/deploy-hub/bin/deploy-hub runner"`.
3. Replace the sudoers rule for `caddy-sync.sh` with the one in step 6.
4. Copy the new `deploy-hub-prune.service`, `systemctl daemon-reload`.
5. Remove what the Telegram integration left behind:
   `systemctl disable --now deploy-hub-bot.service deploy-hub-userops.path`,
   delete their unit files, `/opt/deploy-hub/telegram.env`, `telegram.log`,
   the old `*.sh`/`*.py`/`bot.py` files in `/opt/deploy-hub/bin`, and the
   `TG_TOKEN`/`TG_CHAT_ID` secrets in the app repos (revoke the bot token
   with @BotFather).

To the gated version (any earlier hub):

1. Write `/opt/deploy-hub/hub.conf` (Bootstrap step 5).
2. Add `repo=<owner>/<name>` to every `/opt/<app>/app.conf`; a deploy of an app
   without it is refused.
3. Register every app in `policy/gate.yaml`.
4. In every app repo: set `VPS_HOST` and `VPS_HOST_KEY` (rerun `onboard`, it
   only adds what is missing) and replace the deploy workflow with the stub
   above. A repo that still deploys with its own workflow is refused by the
   runner.
5. Check: `onboard status`, then push a commit to one app and follow the run.
