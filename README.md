# deploy-hub

Push to the default branch of a connected repo → CI runs the **quality gate**
on the commit and the built image; only a commit that passes every check is
pushed and deployed to the VPS, health-checked, with zero-downtime blue-green
cutover and automatic rollback. A rejected commit never reaches the server: the
verdict of every check lands in the deploy journal, structured logs and
metrics. Connecting a new repo is one command (`deploy-hub onboard`). No PaaS,
no daemons: one reusable GitHub Actions workflow, one Go binary on the server,
SSH in between. New to the code? Start with [ARCHITECTURE.md](ARCHITECTURE.md):
what runs where, the life of a deploy, why it is built this way and the
pitfalls. When things break, see [RUNBOOK.md](RUNBOOK.md).

The VPS address is configuration, not code: `VPS_ADDR` on the operator machine,
the `VPS_HOST`/`VPS_HOST_KEY` secrets in each app repo and
`/opt/deploy-hub/hub.conf` on the server. Two ways in: the `deploy` user
(locked to the runner — how CI, `onboard status` and `deploy-hub mcp` reach it) and your own root
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
              deploy.yml@main, for the app's own repository, its main branch
              (ref refs/heads/main) and this commit
              lock → docker login (ephemeral job token) → pull <image>@<digest> → logout
              → blue-green: start a standby, health gate (90s), recreate the main
                service (the standby carries the work; for web apps Caddy fails
                over to it), gate it, stop the standby gracefully
                (apps with a writable mount are recreated in place instead)
              → ok:   state advanced, journal line, metrics pushed
              → fail: standby discarded, the current version keeps serving
```

Images are built only in CI, never by the hub on the VPS. For private app
repos CI runs on an isolated self-hosted runner on the same host (a user `ci-<repo>` per repo,
rootless Docker, its own network namespace, CPU/memory/disk caps; see
[Self-hosted CI runner](#self-hosted-ci-runner)); there is no GitHub-hosted
fallback. Load tests never touch the production apps (see below).

## Pull request gate

The same `deploy.yml` runs on `pull_request` into `main` in **gate-only
mode**: the same toolchain, policy and checks as on `main`, on the same runner,
but the job (check name `deploy / gate`) stops after the gate. It receives no
deploy secrets, does not report to the hub, does not push the image and does
not deploy; the hub itself refuses an OIDC token whose `ref` is not
`refs/heads/main`. The verdict is the PR check status, a table of every check
in the job summary and the `gate-reports` artifact.

```
pull request into main
  → caller stub (concurrency group <stub name>-gate-<PR number>, a new push cancels the old run)
    → deploy.yml, job "gate": toolchain → render → docker build → deploy-hub gate
      → job summary + gate-reports; nothing is pushed, reported or deployed
push to main
  → job "gate and deploy" (the deploy above); while it waits for the runner,
    deploy-hub-preempt.timer on the hub host cancels the queued or running
    pull request gates of the repo (any stub, they share the runner) and
    re-runs them once the deploy holds the runner (see Self-hosted CI runner)
```

A main deploy therefore never waits behind a pull request gate; a gate that a
deploy interrupted starts again from scratch after it.

**Merge only on a green gate.** Merge a pull request only when every gate
check (`deploy / gate`, and `deploy-<app> / gate` for a second app) passed for its latest commit, and only by merging the pull request (no direct
pushes to `main`):

```
gh pr checks <n> -R DreamsAreReal/<app> --watch --fail-fast && gh pr merge <n> -R DreamsAreReal/<app> --squash
```

Branch protection and rulesets are not available for private repos on GitHub
Free (the API answers 403), so the rule is enforced by this command in agents'
and people's workflow, not by GitHub. When the repo moves to a plan with
protection, make `deploy / gate` a required status check of `main`.

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
| `hygiene` | state/backup/debug/report files, keys, scanner-suppression files tracked in git, except the exact paths an app lists in `allowed_files` | — |
| `comments` | any comment or documentation string in source and configuration files of any language (Python docstrings included); prose (`.md`, `.txt`, …) and tool-generated lockfiles are not source, `// indirect` is the only note allowed in `go.mod` | go/scanner, YAML parser, chroma lexers |
| `markers` | work markers (the four listed in the policy) anywhere in tracked files | — |
| `language` | a letter outside the Latin script in a tracked file, except under the app's `localized` paths (UI texts and their tests) | — |
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
| `workflows` | any finding in `.github` at the auditor level (the hub accepts one, see below) | zizmor, actionlint |
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
project, fails the `stacks` check. Files under a `testdata` directory are
fixtures, not code: no pack claims, lints, covers or mutates them, and the
duplication check does not count them.

| Stack | Project manifest | Tools |
|---|---|---|
| Go | `go.mod` | go (`mod verify`, `list -deps`, `test -race -cover`, `test -fuzz`), golangci-lint, gocognit, deadcode, gobco, govulncheck, the built-in mutation engine (isolated build cache in the report directory) |
| Python | `pyproject.toml`, `requirements*.txt`; loose `.py` files form a root project | uv, ruff, mypy `--strict`, complexipy, vulture, pytest with coverage (branches, per-test contexts), hypothesis, trivy, the token mutation engine |
| Node / TypeScript | `package.json` | npm, eslint (typescript-eslint strict, sonarjs), `tsc --strict`, knip, vitest / jest / mocha / `node --test`, fast-check, trivy, the token mutation engine |
| .NET | `*.sln`, `*.slnx`, `*.csproj`, `*.fsproj`, `*.vbproj` (the outermost one) | dotnet (`restore --locked-mode`, build with the recommended analyzers and SonarAnalyzer as errors, `format whitespace`, `test` with coverlet), CsCheck / FsCheck, trivy, the token mutation engine |
| Shell | none; `.sh` and `.bash` files form a root project | shellcheck (every optional check), shfmt, bats with bash xtrace coverage, the token mutation engine |
| Static site | none; `.html`, `.htm` and `.css` files form a root project | html-validate (recommended rules, SRI on cross-origin resources; Go `html/template` files are rendered first), stylelint standard |
| SQL | none; `.sql` files form a root project | sqlfluff (parse only) |

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
  tests. eslint knows the browser, service worker and Node globals; anything
  else (an embedding runtime's API, say) is reached through `globalThis`. A scenario names a test as `path/to/file.test.ts > test name`.
  Linters, `tsc` and knip come from the hub's lock in `policy/node`. With
  `node --test`, each test file first runs alone with coverage; a mutant then
  runs only the test files that executed its line (the whole suite when none
  did), so the time per mutant follows the tests that matter to it.
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
  An `.html` or `.htm` file inside a Go module that contains `{{` is a Go
  `html/template` file. html-validate never sees it raw: the files of one
  directory form a template set, parsed with Go's own template parser. Each
  template no other template calls is rendered without data: a
  `{{template}}` call inlines its target, every branch of `if`, `with` and
  `range` (an empty `else` too) appears in at least one rendering, a `range`
  body appears twice, and an action prints a unique number wherever markup
  takes a value. The renderings get the same html-validate rules, and each
  finding points back at the template file and line. A template that does
  not parse, or calls a template the set does not define, is a
  `go-template` finding.
- **SQL.** Schema and migration scripts are data for the program that runs
  them, and that program's tests cover them: only `lint` applies, every other
  check is `n/a`. A file passes when it parses in one of the sqlite,
  postgres, mysql, tsql or ansi dialects; otherwise the parse errors of the
  sqlite dialect are the findings. sqlfluff comes from the hub's hashed lock in
  `policy/sql`.

Approved dependencies in the policy use the Go module path for Go and a stack
prefix for the rest: `pypi:requests`, `npm:express`, `nuget:Serilog`. When a
package publishes no machine-readable license, `declared_licenses` in the
policy records it by package URL without a version
(`pkg:npm/some-lib: MIT`); the license must be on the approved list.
A license file whose text syft cannot identify shows up as `sha256:<digest>`;
after a person reads it, `reviewed_license_texts` maps that digest to an SPDX
expression built from approved licenses, or to `NONE` when the file grants no
license for shipped code (a logo attribution, say). A changed text gets a new
digest and needs a new review. License files under `testdata/` of a Go module
are ignored: the Go toolchain never builds those directories.

**Whose code.** Repositories of the policy `owners` that are not forks get every
check. Forks and other code get only the security checks (secrets, SAST,
dependencies, licenses, workflows, image, DAST); the quality checks are
reported as `n/a`. Checks that need an image or an HTTP surface are `n/a` for
app kinds without one (`binary`, `worker`). Nothing else is ever skipped, and a
check that cannot finish (tool crash, timeout) fails. Each check gets
`check_timeout`; mutation testing gets `long_check_timeout`.

**Mutation testing** is a built-in engine with every mandatory operator:
condition inversion, comparison and boundary changes, arithmetic and logical
operator swaps, `++`/`--`, removed negations, changed return values, removed
statements and disabled error handling. Before mutating, every test runs alone
once with coverage over all production packages, which gives the tests that
execute each line. Each mutant is compiled with `go test -overlay` and run
against only those tests, wherever they live; a line outside every coverage
block runs its own package and every package that depends on it, and a
package with a test that fails when run alone always runs whole. A smaller
selection can only lower the score. Mutants on lines no test executes count as survivors;
mutants that do not compile are not mutants and are not counted. There is no
way to exclude a mutant. Every mutant's test run is its own process group with
a memory budget (the memory the gate may use divided by the workers plus one); a run
that exceeds it is killed and the mutant counts as detected (`out-of-memory`),
like a timeout, so a mutant that turns a loop into an endless allocation cannot
take the CI runner down. The language packs mutate with the same engine and the
same budget. "The memory the gate may use" is `MemTotal` capped by the tightest
`memory.max` along the gate's cgroup v2 chain, and the workers are half the
CPUs, at least two, where the CPUs are `NumCPU` capped by the tightest
`cpu.max` quota. On the self-hosted runner the repo's slice (9G, 6 CPUs) sets
both, not the host.

Disk: the baseline runs and the test index build into one Go build cache in the
report directory. Each mutation worker then gets its own copy made of hard
links to it (no extra space); once the builds a worker added pass 1 GiB they are
deleted after its current mutant, so the build cache stays within its baseline
size plus 1 GiB per worker however many mutants run, while consecutive mutants
of a worker still reuse each other's builds. Before it starts, after the baseline runs,
after the test index and then every 15 seconds, the run measures its report
directory (the build caches included) (hard links counted once) and
the free space of that disk; above `mutation_disk_gib` (20) or below
`mutation_free_gib` (10 GiB free) it stops with `mutation testing stopped to
protect the disk` and the numbers, keeps the verdicts it finished, and the
check fails as incomplete.

**Verdict cache (incremental mutation, Go).** Every Go mutant gets a key: the
sha256 of everything its verdict depends on. That is the mutant itself (file,
line, operator, edits); the build closure of every test binary that links its
package, test-only imports included, from `go list -deps -test` (every file in
each package directory of the module, its embedded files and `testdata/` tree;
`path@version` for external modules, the Go version for the std library); the
files, directories and environment variables those tests actually read in the
baseline run (Go's `-test.testlogfile`, only paths inside the checkout count,
read through `os.Root`, so a symlink cannot point elsewhere); every non-Go file
of the module when a package of that test binary has a `TestMain` or an
`init` function, because the test log starts only inside `m.Run`; `go.mod`,
`go.sum`, `go.work`, `go.work.sum` and `vendor/modules.txt`; `go env` (Go
version, target, cgo, `GOFLAGS`, `GOEXPERIMENT`, compilers and their flags);
the engine's own test flags; the app policy, thresholds and limits; the host
(CPUs, mutation workers and the memory budget per mutant, since `timed-out`
and `out-of-memory` depend on them); and `EngineVersion`. `EngineVersion` is the
sha256 of the engine sources (`internal/gate/mutation` and the module packages
it imports, except `policy` and `validate`, whose effect is already in the
policy part). `deploy-hub engine-version` computes it in the checkout and
`make build` and `make toolchain` link it in with `-ldflags -X`, so every change
to those sources is a new engine. A binary built without it (`go run`, `make
gate`) neither reuses nor stores any verdict. When anything in a mutant's key
changes, the mutant runs again. A package whose tests left no test log is never cached.
What stays outside the key is a read made by a package-level variable
initializer or by a child process of a test; the scheduled full run without the
cache is the backstop for those. A run that stops early (an errored mutant, the
check timeout) still keeps the verdicts it finished; a mutant whose run was cut
off by the cancellation is never kept.
`deploy-hub gate --mutation-cache FILE --mutation-cache-sha256 sha256:HEX` takes
verdicts from an earlier run: the file must lie outside the checkout. If its
sha256 differs from the one given, or it cannot be read, it is ignored and every
mutant runs. A cached verdict counts in the score exactly like a fresh one. Each
entry of `mutation.json` says `"source": "cache"` or `"run"`, and the check
reports `mutants_cached` and `mutants_run`. After every run the gate merges
the verdicts of all Go projects into `<out>/mutation-verdicts.json` and prints
its sha256. `deploy.yml` does not pass a cache yet, so in CI every run is
still a full one.

**Load test, done for a 1-CPU VPS.** The image runs in an isolated docker
network on the CI runner with the production limits from the policy (`cpus`,
`memory`, no swap, pid limit); a built-in generator sends the app's profile
(`rate`, `duration`, `concurrency`, `paths`) and the check fails on p95 above
the limit, errors above the limit, a crash, a restart or an OOM kill. The VPS
itself is never load-tested.

**Data volumes.** A service lists the directories it keeps state in under
`data`, the same container paths its production compose mounts. The stage
mounts a fresh named volume on each one and removes it with the container, so
the load test and the scan hit the app as it runs in production, not a
fallback for a missing volume.

**A stage that never comes up.** When the stage publishes no loopback port or
never answers its health path, the gate inspects the container before removing
it: the check's reason and the `gate stage failed` log line carry its state
(`running`, `oom`, `restarts`, `exit`) and the last 50 lines of `docker logs`,
the reason keeping the tail of them.

**DAST.** The same isolated stage runs an OWASP ZAP full scan (or the API scan
when the app declares an OpenAPI path) for the time budget in the policy; any
alert of Low risk or above blocks the deploy.

**Allowed files.** `allowed_files` lists exact paths (same pattern rules as
`localized`) that an app may track although they match `forbidden_files`, for
example the working notes `docs/STATE.md` of an app built with agents. Keys,
`.env` and suppression files stay forbidden everywhere else.

**Localized paths.** An app whose interface is not in English lists where its
UI texts and their tests live: `dir/` matches that directory at any depth, a
pattern without `/` matches the file name, any other pattern matches the whole
path. Everything else stays English-only.

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
    data: [/data]            # container paths of its production volumes
    localized: [internal/texts/, "*_test.go"]  # UI texts and tests that may use another language
    allowed_files: [docs/STATE.md]             # tracked although forbidden_files matches it
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

**The one accepted workflow finding.** The hub's own gate accepts exactly one
finding, set in `policy/gate.yaml` under `apps.deploy-hub.accepted_workflow_findings`
with its reason: zizmor `self-hosted-runner` (Medium) on the `runs-on` line of
`.github/workflows/deploy.yml`, which picks the
[self-hosted CI runner](#self-hosted-ci-runner) for private app repos (owner
decision of 2026-10-22, tasks #600, #621, #627, #633). An accepted finding
names the rule, the severity, the exact file and the YAML key at the reported
line, and the policy refuses one without a reason that cites a task. The
`workflows` check skips a zizmor finding only when all four match, and only
once per entry: a second self-hosted job, another severity, rule, file or line
fails the check, and so does an entry that zizmor no longer reports. No other
check reads this list, and `TestRepositoryPolicyAcceptsOneFinding` fails when
the policy holds any other entry, for the hub or for an app.

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
| `engine-version` | build (make) | sha256 of the mutation engine sources, linked into the binary |
| `caddy-sync` | root (via a narrow sudoers rule) | regenerates the Caddyfile from `apps.list` |
| `host-sync <app> <tag>` | root (via a narrow sudoers rule, called by `runner`) | installs the host files of an app's gated image, as listed in the policy (see [Host files](#host-files-host-sync)) |
| `self-update [sha-<commit>]` | root (systemd timer every 10 minutes) | replaces the binary with the newest gated release of this repo (see [Releases and self-update](#releases-and-self-update)) |
| `sudoers-sync` | root (called by `self-update`) | writes `/etc/sudoers.d/deploy-hub` from `hub:` in the policy |
| `ci-runner-sync` | root (called by `self-update`) | installs the embedded `server/ci-runner` into `/opt/deploy-hub/ci-runner` and runs `ci-runner-setup host` when it changed |
| `version` | anyone | prints the commit the binary was built from |
| `prune` | root (systemd timer) | keeps the running image + 2 previous per app |
| `provision --app … ` | root (from `onboard`) | prepares `/opt/<app>/` and the allowlist line |
| `facts <app>` | root (from `onboard`) | reports what already exists on the server |
| `onboard <repo> …` | operator machine | connects a GitHub repo end-to-end |
| `mcp` | operator machine | MCP server for AI agents: `status`, `history`, `rollback` over the deploy channel |

`make build` builds it for linux/amd64 into `$TMPDIR/deploy-hub-build/`, outside
the repo, for local testing; `make build COMMIT=<sha>` stamps the commit that
`version` prints. The binary that runs on the server is never built by hand:
only CI publishes it (see [Releases and self-update](#releases-and-self-update)).
On the operator machine use `go run ./cmd/deploy-hub onboard …`.

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

5. **Hub directory, server config, GitHub token and binary** (`<host-slug>`
   is the address with dashes, as used by sslip.io, e.g. `203-0-113-7`).
   The repo is private, so the hub reads releases and CI runs with a
   fine-grained PAT of the owner, limited to `deploy-hub` (and `tasks`, whose
   history-guard dispatch shares it) with **Contents** and **Actions**
   permissions. It lives only on the server, root-owned, mode 600:

   ```
   ssh vpn bash -s <<'EOF'
   set -euo pipefail
   install -d -m 755 /opt/deploy-hub/bin
   install -d -m 750 -o deploy -g deploy /opt/deploy-hub/logs
   install -d -m 700 /etc/deploy-hub
   printf "host_slug=<host-slug>\n" > /opt/deploy-hub/hub.conf
   touch /opt/deploy-hub/apps.list /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock /opt/deploy-hub/last-error.log
   chown deploy:deploy /opt/deploy-hub/deploys.log /opt/deploy-hub/deploy.lock /opt/deploy-hub/last-error.log
   EOF
   printf '%s\n' '<github_pat_…>' | ssh vpn 'install -m 600 -o root -g root /dev/stdin /etc/deploy-hub/github.token'
   ```

   This is the **only time a person installs the binary**; from then on the
   hub updates itself (step 7). From a checkout of this repo on the operator
   machine:

   ```
   go run ./cmd/deploy-hub bootstrap            # or: bootstrap sha-<commit>
   ```

   `bootstrap` reads the token over root SSH (`VPS_HOST_ALIAS`, default
   `vpn`), takes the newest release (or the given one) and makes the same
   checks as `self-update`: tag `sha-<40 hex>` targeting that commit, a green
   `ci.yml` push run on `main` for it, the published sha256. It streams the
   binary to `/opt/deploy-hub/bin/deploy-hub.new`, moves it into place only if
   it prints that commit from `version`, and removes it otherwise. It refuses
   to run when `/opt/deploy-hub/bin/deploy-hub` already exists (the hub
   updates itself) and stops before any GitHub request when the token file is
   missing, empty, not root's or readable by others; a wrong or expired token
   fails with GitHub's `401`.

   `hub.conf` also accepts `acme_email=` (default `admin@<host-slug>.sslip.io`);
   the environment variables `HOST_SLUG`/`ACME_EMAIL` override it.
   Optional `delivery_url=<tracker>/api/deliveries/<secret>` reports every
   gate verdict, deploy and rollback to the task tracker as delivery steps
   (see [Delivery steps](#delivery-steps)); `DEPLOY_HUB_DELIVERY_URL` overrides it.

6. **sudoers rule** — the runner (user `deploy`) may run only the root
   commands listed under `hub:` in `policy/gate.yaml` (`caddy-sync`,
   `host-sync`, `self-update`). The binary writes the rule itself, checks it
   with `visudo -cf` and keeps the previous one if the check fails:

   ```
   ssh vpn '/opt/deploy-hub/bin/deploy-hub sudoers-sync && cat /etc/sudoers.d/deploy-hub'
   ```

   `caddy-sync` and `self-update` are allowed without arguments (`""`),
   `host-sync` with any (`*`): it takes exactly `<app> <tag>`, both checked
   against the same regexes as the runner's, and does nothing for an app
   without a `host:` spec in the policy compiled into the binary. Every
   `self-update` rewrites the rule from the new binary's policy.

7. **Timers**: daily prune, the per-minute status refresh (keeps the metrics
   continuous, see Observability) and the self-update every 10 minutes. Copy
   the unit files of `server/systemd/` from the same commit as the release you
   installed in step 5:

   ```
   tar -C server/systemd -cf - . | ssh vpn 'tar -C /etc/systemd/system -xf - --no-same-owner'
   ssh vpn 'systemctl daemon-reload && systemctl enable --now deploy-hub-prune.timer deploy-hub-status.timer deploy-hub-self-update.timer deploy-hub-preempt.timer'
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
   ssh vpn 'systemctl enable --now deploy-hub-history-guard.timer'
   ```

   (The unit files arrived with step 7.)

   Check: `ssh vpn 'systemctl start deploy-hub-history-guard.service'` exits 0
   and `gh run list -R DreamsAreReal/deploy-hub -w history-guard.yml -L 1`
   shows a fresh `workflow_dispatch` run. When the PAT expires, rewrite the
   header file the same way.

9. **Observability** (recommended): the shared logs/metrics stack lives in its
   own repo, [DreamsAreReal/observability](https://github.com/DreamsAreReal/observability).
   Load the hub's alert rules into it (see "Alerts" under Observability).

10. **Self-hosted CI runner** (every repo, deploy-hub included; there is no
    GitHub-hosted fallback): see
    [Self-hosted CI runner](#self-hosted-ci-runner).

Verify the hub answers: `VPS_ADDR=<vps> go run ./cmd/deploy-hub onboard status`
prints a header (an empty app list is fine on a fresh hub).

## Releases and self-update

The binary on the server comes only from this repo's CI, and only for a commit
that passed the full gate on `main`:

1. `ci.yml` runs the gate on every push to `main`, then builds the linux/amd64
   binary with the commit stamped in (`deploy-hub version` prints it; the
   build step fails if it does not).
2. The `release` job (push to `main` only, after `gate`) publishes the release
   `sha-<commit>` targeting that commit, with the assets
   `deploy-hub-linux-amd64` and `deploy-hub-linux-amd64.sha256`.
3. On the server `deploy-hub-self-update.timer` runs `deploy-hub self-update`
   as root every 10 minutes. It reads releases and runs with the token in
   `/etc/deploy-hub/github.token` (Bootstrap, step 5; without it the run fails
   before any request), takes the latest release (or the one given as
   `sha-<commit>`) and installs it only when:
   - the tag is `sha-<40 hex>` and the release targets exactly that commit;
   - both assets are attached to that release and served by the API of this
     repo (`api.github.com/repos/DreamsAreReal/deploy-hub/releases/assets/<id>`;
     the token is not sent on to the storage host GitHub redirects to);
   - GitHub lists a successful `ci.yml` run for that commit, event `push`,
     branch `main`;
   - the release commit descends from the installed one (GitHub compare
     `ahead` or `identical`), so a rewritten `main` is never installed; this
     replaces the branch protection the private repo lacks on the free plan.
     A release pinned by tag skips this check (that is how a rollback works);
   - the downloaded binary matches the published sha256;
   - the candidate (`/opt/deploy-hub/bin/deploy-hub.new`) reports that commit
     from `version`.
4. It keeps the current binary as `deploy-hub.prev`, renames the candidate into
   place, then checks the installed binary: `version` must print the commit,
   `sudoers-sync` (from the new binary's policy) must pass `visudo -cf` and
   `ci-runner-sync` must install the binary's CI runner files. If any of them
   fails, it copies `deploy-hub.prev` back and runs its `sudoers-sync` and
   `ci-runner-sync`.
5. `ci-runner-sync`: the binary embeds `server/ci-runner` of its commit (hooks
   `ci-job-started`, `ci-runner-clean`, units, `ci-runner-setup`), so the runner
   files ship in the same gated release. When `/opt/deploy-hub/ci-runner`
   differs from the embedded tree (content, mode, an extra or a symlinked file),
   it writes the tree to `ci-runner.new`, keeps the current one as
   `ci-runner.prev`, swaps them and runs `ci-runner-setup host` from the new
   tree. If `host` fails, it puts `ci-runner.prev` back and runs its `host`
   again. `host` restarts the runners (`try-restart`), so a job running at that
   moment fails; this happens only when the runner files change.

Every installed or refused release is journaled (`deploy-hub@<sha7> self-update
ok|fail`, a restore adds `restored=ok|fail`); a run that finds the installed
commit only rewrites the sudoers rule and the CI runner files if they drifted
(an installed runner tree is journaled like a release). Metrics:
`deploy_hub_self_update_success` and `deploy_hub_self_update_installed`; the
alert `DeployHubSelfUpdateFailed` fires when no run succeeded for 40 minutes.
To pin an older gated release, run `sudo /opt/deploy-hub/bin/deploy-hub
self-update sha-<commit>`; the next timer run moves back to the latest one.
A release older than `ci-runner-sync` cannot be pinned: its binary fails that
check and the current one is restored.
After a deliberate rewrite of `main`, the timer refuses the new releases
(`release commit does not descend from the installed one`) until an operator
installs one by tag the same way.

The only manual install is the bootstrap (Bootstrap, step 5). Do not copy a
binary to the server any other way.

## Connecting a repo — one command

The app repo needs a `Dockerfile` (one built image per app) and an entry in
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
`--mem`, `--health-path`, `--app`, `--dockerfile`, `--compose`, `--ref`.

`--ref <branch>` reads the Dockerfile, compose and caller stub from that branch
instead of the default one — for a repo whose hub files are still in an unmerged
PR (merging deploys). The stub must already be on the branch, byte-for-byte as
below (triggering on the default branch); `--ref` never commits it and stops
before any change when it is missing or differs.

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
  pull_request: {branches: [main]}
permissions: {}
concurrency:
  group: ${{ github.event_name == 'pull_request' && format('deploy-gate-{0}', github.event.pull_request.number) || 'deploy' }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
jobs:
  deploy:
    name: deploy
    permissions: {contents: read, packages: write, id-token: write}
    uses: DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@main
    with: {app: myapp}
    secrets:
      VPS_SSH_KEY: ${{ github.event_name != 'pull_request' && secrets.VPS_SSH_KEY || '' }}
      VPS_HOST: ${{ github.event_name != 'pull_request' && secrets.VPS_HOST || '' }}
      VPS_HOST_KEY: ${{ github.event_name != 'pull_request' && secrets.VPS_HOST_KEY || '' }}
```

The caller grants one permission set to the reusable workflow; its only job
uses `packages: write` and `id-token: write` in the push and deploy steps,
which do not run on a pull request. The GITHUB_TOKEN never gets `contents: write`
or `actions: write`: the hub release uses its own secret (see Releases and
self-update) and preemption runs on the hub host. Pull requests
get no deploy secrets (see [Pull request gate](#pull-request-gate)).

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
- `mem_limit: 256m`, json-file log rotation, `restart: unless-stopped` and
  `stop_grace_period: 30s` are added only where you did not set them; `.env`
  is attached to the main service.
- `deploy-hub.overlap: "false"` on the main service turns off the zero-downtime
  overlap of old and new instance (see Health gate and rollback).
- sidecars are started with `--no-recreate` and then brought up once more with
  `--no-deps` and the list of every service but the main one, so a sidecar
  whose image or settings changed in the compose is recreated, an unchanged one
  keeps running and the main service is never touched outside the blue-green
  swap. A rollback runs the compose that is installed now (BYOC keeps no copy
  of older composes).

A second app of the same repo (for example `stt` next to `tasks`) brings its
own compose anywhere in the repo and names it in the caller's `compose:` input
(a relative path inside the repo, like `dockerfile:`); without the input CI
takes the root compose as above. `build.context` and `build.dockerfile` in that
file stay relative to the repo root, not to the file's directory as plain
`docker compose` reads them: `stt/docker-compose.yml` says `context: .` and
`dockerfile: stt/Dockerfile`. Register
the app in `policy/gate.yaml` with the same `repo:`: the `registration` check
and the `repo=` binding of `app.conf` allow several apps per repo, and the gate
checks the whole repo for each of them. Onboard it with
`onboard <repo> --app stt --dockerfile stt/Dockerfile --compose stt/docker-compose.yml`:
the profile and port come from that compose, and the stub goes to
`.github/workflows/deploy-stt.yml` (workflow name and concurrency group
`deploy-stt`) so the first app's `deploy.yml` stays as it is.


### Host files (host-sync)

Some apps need files on the host itself, owned by root: a systemd timer that
collects host metrics, a backup script, a Caddy site in `/etc/caddy/extra/`.
The hub installs them from the app's **gated image**, never from the network
or git, and only to the paths the **policy of this repo** lists for the app:

```yaml
apps:
  observability:
    host:
      files:
        - {src: obs-host-metrics, dst: /usr/local/sbin/obs-host-metrics, owner: root, group: root, mode: "0755"}
        - {src: obs-host-metrics.timer, dst: /etc/systemd/system/obs-host-metrics.timer, owner: root, group: root, mode: "0644"}
        - {src: ingest.caddy, dst: /etc/caddy/extra/observability-ingest.caddy, owner: root, group: caddy, mode: "0640", env: ingest.env}
      dirs:
        - {path: /var/lib/node_exporter/textfile, owner: root, group: root, mode: "0755"}
      timers: [obs-host-metrics.timer]
      checks: [obs-host-metrics.service]
```

- `src` is a path inside `/deploy-hub/host/` of the image. The image must hold
  exactly the listed files there: an unlisted file, a symlink, a FIFO or a
  file over its size limit fails the sync. An executable (an x bit in `mode`,
  no `env:`) may be up to 64 MiB. It is streamed and compared by sha256, and
  if it changes between the check and the write, the sync is refused. Text
  files and `env:` templates are limited to 1 MiB.
- `dst` must be a file name directly inside one of `/usr/local/sbin/`,
  `/etc/systemd/system/`, `/etc/caddy/extra/` or `/var/lib/node_exporter/`
  (the list is in code, `policy.HostRoots`); `mode` is four octal digits
  without setuid/setgid/sticky bits and without group or other write.
  `timers` and `checks` may only name units the same spec installs. The
  policy loader rejects anything else, so the app repo cannot widen it.
- `env` renders `@KEY@` placeholders from `/opt/<app>/<env>`; every value
  must match `^[A-Za-z0-9._:/-]{1,256}$` (no spaces, braces or newlines) and
  the env file may not be a symlink.

When it runs: `runner` calls `sudo -n deploy-hub host-sync <app> <tag>` after
the containers of a deploy, rollback or redeploy passed the health gate, for
apps that have a `host:` spec in the policy compiled into the server binary.
`host-sync` (root):

1. refuses unless `deploys.log` has `<app>@<sha7> gate ok` and the local image
   `<image>:<tag>` was pulled by digest from `image=` of `app.conf`; it then
   reads the files from `<image>@<digest>` (`docker create` + `docker cp`, the
   container never starts);
2. refuses a destination that is a symlink or not a regular file, and a
   destination directory that is a symlink or missing;
3. copies the previous version of every file it is about to change to
   `/var/lib/deploy-hub/host-backup/<app>/<UTC stamp>-<sha7>/` (`manifest`
   lists mode, uid, gid and path; the newest 5 copies are kept);
4. writes each file next to its destination (`O_EXCL|O_NOFOLLOW`), sets owner
   and mode, and renames it into place;
5. activates: `systemd-analyze verify` and `systemctl daemon-reload` when a
   unit changed, `caddy validate` and `systemctl reload caddy` when a Caddy
   site changed, `systemctl enable --now <timers>`, then `systemctl start
   <checks>`, which must succeed: that is the health check of the host part.

If any step of 4–5 fails, the previous files are put back (new ones are
removed), systemd and Caddy are reloaded, the journal gets `host-restore`,
and the runner treats the deploy as failed: it rolls the containers back to
the previous tag (or stops a first deploy). Every run writes `<app>@<sha7>
host-sync ok|fail Ns changed=<n>` to the journal and pushes
`deploy_hub_host_sync_success{app}` and `deploy_hub_host_files_changed{app}`.

The host spec ships with the binary (`policy/gate.yaml` is embedded), so a
change to it reaches the server only with a new binary release.

## Self-hosted CI runner

`deploy.yml` runs its only job on `[self-hosted, linux, eu]`: a GitHub Actions
runner on the hub host, one per app repo, including deploy-hub itself (its
`ci.yml` calls `deploy.yml`). All repos are private and GitHub-hosted runners are
not paid for, so there is no fallback runner: while a runner is down, jobs wait
in the queue (alert `DeployHubCIRunnerOffline`). Pull requests from forks reach
the runner only to be refused by the job-started hook.

zizmor reports this `runs-on` as `self-hosted-runner`; it is the one finding
the hub's gate accepts (see
[the one accepted workflow finding](#the-quality-gate)).

A runner is registered on one repository (personal accounts have no runner
groups), so only that repo's jobs reach it. Keep "Run workflows from fork pull
requests" off in the repo's Actions settings (the default for private repos).

Isolation, all in [`server/ci-runner`](server/ci-runner):

| What | How |
|---|---|
| user | one per repository: `ci-<repo>`, system user with its own subordinate uids/gids, no sudo, no login shell, home `/var/lib/ci/<repo>` (mode 700; `/var/lib/ci` itself is root, 755, because the runner refuses to start unless it can list every directory above its own), not in the `docker` group. A job of one repo cannot read or change the other runner's files (`runner/`, `.credentials`, `_work`), Docker data or socket, or signal and read `/proc/<pid>/environ` of its processes |
| Docker | rootless `dockerd` per repository (`ci-docker@<repo>`, user `ci-<repo>`), data in `/var/lib/ci/<repo>/docker`, socket `/run/ci/<repo>/docker.sock` (directory 700); the system socket is hidden and not accessible |
| files | `ci-runner@<repo>`: `ProtectSystem=strict`, `ProtectHome`, `NoNewPrivileges`, empty capability set; `/opt`, `/etc/deploy-hub`, `/etc/srv-backup`, `/etc/wireguard`, `/var/lib/docker`, backups are inaccessible. The rootless daemon and the user manager of `ci-<repo>` (it holds the container scopes) are jailed the same way, minus `NoNewPrivileges` for `dockerd` (rootlesskit needs the setuid `newuidmap`) |
| network | one namespace per repository, `ci-<repo>` (`ci-netns@<repo>`): the runner, its rootless `dockerd` and the user manager of `ci-<repo>` all run in it. Its own veth pair (`cih<uid>` on the host, `cin<uid>` inside) and a /30 out of `10.79.0.0/16` picked by the uid of `ci-<repo>` (host side `.1`, namespace `.2` of the block `10.79.0.0 + 4 × uid`); NAT to the internet only. The host loopback (Caddy admin, VictoriaMetrics/Logs, Bugsink), the `shared` and other Docker networks, private ranges and the metadata address are dropped; of the host only SSH/HTTP/HTTPS on the public address answer. The drops live in the nftables table `inet ci_netns`, which neither Docker nor an iptables reload touches, and a drop there is final whatever Docker puts first in `FORWARD`; its rules match every host-side veth by the name prefix `cih*`, so a new repo needs no rule change. Ports published by gate containers (rootlesskit's builtin port driver) live on the repo's own namespace, so a job of one repo cannot connect to the app, database or DAST target of another, take its port first, or reach its abstract unix sockets; the other namespace's addresses are in `10.0.0.0/8` and dropped |
| which jobs | job-started hook `ci-job-started`: workflows of the repo's own `main` (push, `workflow_dispatch` or `schedule`) and pull requests into `main` from the repo itself (`refs/pull/<n>/merge`, head repo checked in the event payload); other branches, fork pull requests and other events fail before their first step. Pull request jobs get no deploy secrets, and a main deploy cancels them ([Pull request gate](#pull-request-gate)) |
| CPU, memory | every repo has its slice `user-<uid of ci-<repo>>.slice` (its runner, its `dockerd` and its user manager with the containers), each with `ci-limits.conf`: 6 of 8 CPUs, CPU and IO weight 20 (production keeps priority), `MemoryHigh` 8G, `MemoryMax` 9G, no swap. Two jobs at once take at most 18G of the host's 23G; the gate sizes its mutation workers from the cgroup limit |
| disk | `/var/lib/ci` is the ext4 image `/srv/ci.img` (240G; `CI_DISK_SIZE` for a new one): CI can never fill `/` |
| disk quota | each repo has a hard ext4 project quota on `/var/lib/ci` (project id = uid of `ci-<repo>`, limits in GiB in `server/ci-runner/ci-disk-quota`: tasks 140, router-bot 40, observability 10, deploy-hub 35), so one repo cannot fill the disk of the others; `register` refuses a repo without a line there. Writes over the quota fail with `Disk quota exceeded`; the job hook prints an error with the usage, and `ci-job-started` refuses jobs while the quota is 95% full |
| leftover processes | `ci-runner-reap`, run by both job hooks: SIGKILL to every process in the runner's cgroup that is not a descendant of the runner itself (`runsvc.sh` → `RunnerService.js` → `Runner.Listener` → `Runner.Worker` → steps). A step that forks into the background or daemonizes is reparented away from the runner and dies when the job ends, and again before the first step of the next job, so it cannot read the environment (`SSH_KEY`, `GH_TOKEN`) of a later job. A stop or restart of `ci-runner@<repo>` kills the whole cgroup (`KillMode=control-group`) |
| cleanup | job-completed hook `ci-runner-clean`: removes the job's containers, networks, volumes and pushed images, trims the build cache to 15 GB, empties the workspace, `RUNNER_TEMP`, `TMPDIR`, the tool cache and the downloaded actions; above 70% disk it drops all images and the language caches |
| fail-safe | `ci-runner-status` stops every runner when the CI disk is not mounted or the nftables table is missing, and stops the runner of a repo whose namespace is gone, whose veth is down or whose runner, `dockerd` or user manager runs outside `/run/netns/ci-<repo>` (`deploy_hub_ci_isolation_ok` 0) |

Why the units look the way they do (the gate forbids comments in them):

- Container limits (`--cpus`, `--memory`, which the gate's stage containers
  use) work in rootless Docker only with the systemd cgroup driver, so each
  `dockerd` talks to the user manager of `ci-<repo>` (`user@<uid>.service`, kept
  running by linger) over `DBUS_SESSION_BUS_ADDRESS`. That user manager has
  to stay in `user-<uid>.slice`: runc takes the bus owner's uid from that
  cgroup name, and from any other slice every container fails with
  "Interactive authentication required". So the limits sit on the per-user
  slice, and there is no single slice above all CI. That user manager is
  jailed by a drop-in exactly like the runner (network namespace `ci-<repo>`,
  read-only system, hidden paths), otherwise a job could mount the bus into a
  container and start a process outside the jail.
- `ci-docker@` hides `/home` by path instead of `ProtectHome`, because
  `ProtectHome` also hides `/run/user`, where that bus lives; and it has no
  `ProtectKernelLogs`/`ProtectKernelTunables`, because a user namespace may
  mount `/proc` for a container only when nothing in `/proc` is covered.
- Ubuntu 24.04 (`kernel.apparmor_restrict_unprivileged_userns=1`) lets
  only profiled programs create user namespaces; `rootlesskit.apparmor` is the
  profile from the Docker rootless docs.
- One `dockerd` per runner: the job hook removes every container of its own
  daemon, which would otherwise kill a concurrent job of the other repo.
- Units that differ per repo only by uid (the slice, the user manager's bus)
  get instance drop-ins from `ci-runner-setup`:
  `ci-runner@<repo>.service.d`, `ci-docker@<repo>.service.d`,
  `user@<uid>.service.d`, `user-<uid>.slice.d`.
- The subnet of a repo comes from its uid (system uids are below 16384, which
  is how many /30 fit `10.79.0.0/16`), so two repos never share one and
  nothing has to keep an allocation table. The interface names carry the uid
  too, because a name is at most 15 characters and a repo name up to 29.
- `ci-runner-reap` runs as the repo user inside the hook and reads the tree
  from `/proc`: the root of the runner is the topmost ancestor of the hook in
  the same cgroup. Killing the whole cgroup instead would also kill the
  listener and drop the job's result. It first stops (SIGSTOP) every stray
  until all of them are stopped, so a fork loop cannot outrun it, and only
  then kills them. If anything is left after 50 rounds, the job-started hook
  refuses the job. Processes stay in the runner's cgroup
  (`ProtectControlGroups`, no delegation to the runner), with one exception
  listed under "What is not isolated".
- The hooks are installed as `/usr/local/lib/ci-runner/*.sh`: the runner runs a
  hook only with a `.sh`, `.ps1` or `.js` name, through `bash -e -o pipefail`,
  so `ci-runner-clean` turns both off and always exits 0.
- Root never writes into `/var/lib/ci` through a path a CI user controls: the
  runner tarball is kept in `/var/cache/ci-runner` (root, 700) and checked
  against its sha256 before every extract, root creates only
  `/var/lib/ci/<repo>` itself and `ci-<repo>` creates everything inside, the
  runner's dependencies come from apt.
- The runner starts through `bin/runsvc.sh` with `KillMode=control-group`.
  With `KillMode=mixed` the unit stopped as soon as `runsvc.sh` exited, and a
  `Runner.Listener` of the previous start stayed in the cgroup ("remains
  running after unit stopped"), so two listeners served one runner.

What is not isolated: a runner is not ephemeral. Its user owns the runner
directory (the runner updates itself), so code a job runs (the app's tests,
its dependencies) can change the runner's own `bin/`, `.env` or hooks
environment and so reach later jobs **of the same repo** (it cannot reach the
other repo). The same holds for its home (for example units in
`~/.config/systemd/user`) and for a container that mounts the user
manager's bus and starts a process in `user@<uid>.service`, outside the
runner's cgroup. Both repos have one owner, the job-started hook admits only
workflows of `main`, and the tool cache and actions are fetched again
for every job. Ephemeral runners (a just-in-time config per job, which needs a
token with Administration: write on the repo kept on the host) would close
this.

The deploy key is written to `RUNNER_TEMP` (emptied after the job), and the
registry login uses a job-local `DOCKER_CONFIG`. The production path does not
change: the job reaches the hub only as `deploy@<vps>` through the forced
command, with the OIDC token and the image digest.

Install and update: `deploy-hub self-update` installs `server/ci-runner` of the
released commit into `/opt/deploy-hub/ci-runner` and runs `ci-runner-setup host`
(see [Releases and self-update](#releases-and-self-update)); do not copy these
files to the server by hand. Registering a repo needs a token, so it stays a
command, run with the installed script:

```
gh api -X POST repos/DreamsAreReal/<app>/actions/runners/registration-token --jq .token \
  | ssh vpn /opt/deploy-hub/ci-runner/ci-runner-setup register <app>
```

`host` is idempotent: `uidmap`/`slirp4netns`, the AppArmor profile that lets
`rootlesskit` create user namespaces on Ubuntu 24.04, the disk image, `quota` and `linux-image-extra-virtual` (the module `quota_v2`
that ext4 project quotas need), the
nftables table, the units, and for every repo already registered its user,
subordinate ids, home, drop-ins, network namespace and linger (a user manager
still running in another namespace is restarted). `register` does the same for one
repo (`<app>` lowercase, at most 29 characters, so `ci-<app>` is a valid user
name), downloads the pinned runner (sha256-checked), configures it as
`eu-<app>` with the label `eu` and starts `ci-runner@<app>`. The runner updates
itself. To remove one: `systemctl disable --now ci-runner@<app> ci-docker@<app>`,
`systemctl stop ci-netns@<app>`, `loginctl disable-linger ci-<app>`, delete the runner in the repo settings,
`/var/lib/ci/<app>` and the user `ci-<app>`.

Disk quotas: the image needs the ext4 features `quota` and `project` once, which
`tune2fs` sets only on an unmounted image. `ci-runner-setup quota-enable` does
it: it refuses while a job runs (`Runner.Worker`) or a deploy holds
`/opt/deploy-hub/deploy.lock`, stops the runners, their `dockerd` and user
managers, unmounts `/var/lib/ci`, runs `tune2fs -O quota,project -Q prjquota`,
mounts it with `prjquota`, sets every registered repo's project id and limit and
starts the runners again, printing both times. A new image gets the features at
`mkfs`. After that `host` re-applies the limits of `ci-disk-quota` on every run;
without the features it only warns. `df /var/lib/ci/<repo>` shows the quota.

Main deploys overtake pull request gates: each repo has one runner, so a push
to `main` would otherwise wait for a running gate. `deploy-hub-preempt.timer`
runs `deploy-hub preempt` as root every 30 seconds for every repo with a runner
on this host (`/var/lib/ci/<repo>/runner/.runner`). While a `push` run of
`main` is queued, it cancels the active `pull_request` runs of that repo (it
reads each run again right before cancelling and never cancels any other
event); once no `main` run waits, it re-runs each run it cancelled unless a
newer run of the same pull request branch replaced it or the run ended
otherwise. Its state is `/var/lib/deploy-hub/preempt.json`. It uses the token
of `/etc/deploy-hub/github.token`, which needs **Actions: Read and write** on
every repo with a runner. Metrics: `deploy_hub_preempt_success`,
`deploy_hub_preempt_cancelled{repo}`, `deploy_hub_preempt_rerun{repo}` and
`deploy_hub_preempt_reruns_max{repo}` (most re-runs of one gate in the last
24 h); alerts `DeployHubPreemptFailing` and `DeployHubPreemptRerunLoop` (one
gate re-run more than 3 times).

Moving from the shared user `ci` of the first version: `host` stops every runner,
removes the user `ci`, its drop-ins and its files on the CI disk, then each
repo is registered again with the `register` line above (`--replace` takes
over the name `eu-<app>` on GitHub).

Moving from the shared network namespace `ci`: `host` stops every runner and
`dockerd`, removes `ci-netns.service`, its veth `ci-host`, its iptables rules
and the namespace, then starts `ci-netns@<repo>` for every registered repo,
restarts its user manager inside it and starts the runners again. Run it
between jobs: a running job is cut off.

Check it:

```
gh api repos/DreamsAreReal/<app>/actions/runners --jq '.runners[] | "\(.name) \(.status)"'   # eu-<app> online
ssh vpn 'systemctl status ci-runner@<app> ci-docker@<app> ci-netns@<app>'
ssh vpn 'runuser -u ci-tasks -- cat /opt/tasks/.env'                # Permission denied
ssh vpn 'runuser -u ci-tasks -- docker ps'                          # permission denied on /var/run/docker.sock
ssh vpn 'runuser -u ci-tasks -- ls /var/lib/ci/router-bot /run/ci/router-bot'   # Permission denied
ssh vpn 'systemd-cgls -u user.slice'                               # each repo in its user-<uid>.slice
ssh vpn 'ip netns list; /usr/local/lib/ci-runner/ci-netns check <app> && echo ok'   # ci-<app> ... ok
ssh vpn 'ip netns exec ci-tasks curl -m 3 127.0.0.1:<port published by router-bot>'  # Connection refused
```

Logs: the runner and the job hook write to the journal, so they are in
VictoriaLogs as `unit:"ci-runner@<app>.service"` (the rootless daemon:
`unit:"ci-docker@<app>.service"`). State: `ci-runner-status.timer` pushes
`deploy_hub_ci_runner_up{runner}` (1 while the runner holds its connection to
GitHub), `deploy_hub_ci_disk_used_ratio`, `deploy_hub_ci_quota_used_ratio{runner}`
and `deploy_hub_ci_quota_on{runner}` every minute; the alerts
`DeployHubCIRunnerOffline`, `DeployHubCIRunnerSilent`, `DeployHubCIDiskFull`,
`DeployHubCIQuotaHigh` (over 85% of a quota) and `DeployHubCIQuotaOff` watch them, `DeployHubCIIsolationBroken` the fail-safe above.

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
| `/opt/deploy-hub/mutation-cache/<app>/verdicts.json`, `verdicts.sha256` | the app's mutation verdict cache and the sha256 the runner recorded when it stored it (`mutation-cache.lock` serializes uploads) |
| `/home/deploy/.cache/deploy-hub/jwks.json` | last GitHub OIDC signing keys, used when GitHub is unreachable |
| `/opt/<app>/docker-compose.yml` | app stack: 127.0.0.1 ports, mem limit, log rotation |
| `/opt/<app>/app.conf` | `profile=`, `port=`, `health_path=`, `image=`, `repo=`, optional `bluegreen=on\|off` (BYOC: `mode=byoc`, `main=`, `hmode=`) |
| `/opt/<app>/.env` | app secrets (600), referenced by compose, never in git |
| `/opt/<app>/.deploy-state` | `current=`/`previous=` sha tags — rollback source of truth |
| `/var/lib/deploy-hub/host-backup/<app>/` | root, 700: previous host files replaced by `host-sync`, newest 5 runs |

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
mutation-cache-get <app> <tag>   # the app's mutation verdict cache
mutation-cache-put <app> <tag>   # merge new mutation verdicts into it
```

`deploy`, `report` and the two `mutation-cache` requests read `key=value` lines on stdin. All of them require `oidc` — a
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

`mutation-cache-put` requires `verdicts` (base64 of the `mutation-verdicts.json`
the gate wrote). Only final verdicts with sha256 keys are accepted; they are
merged into the stored cache under its own lock, written atomically and their
sha256 recorded next to them. Over 100,000 verdicts, only the latest upload is
kept, and a file over 16 MiB is never written, so an app's directory under
`/opt/deploy-hub/mutation-cache` stays within a few tens of MiB. One upload is limited by the 8 MiB stdin line to about 75,000 verdicts.
`mutation-cache-get` prints `sha256=<hex>` and `verdicts=<base64>`, or nothing
when there is no cache yet. The runner serves the file only if its sha256 still
matches the recorded one, so a cache changed on disk outside the runner is not
served (`deploy_hub_mutation_cache_intact` 0, alert
`DeployHubMutationCacheCorrupt`); the next upload replaces it. App repositories
have no other way to reach the cache: it is not in the repo, not in the GitHub
Actions cache, and only the hub workflow passes the identity check.

`rollback`/`redeploy` accept only versions this hub deployed successfully
before (current, previous or a successful deploy in the journal), so an image
that never passed the gate cannot be started by hand through the runner.

Runner settings: `DEPLOY_HUB_OIDC_AUDIENCE` (default `deploy-hub`),
`DEPLOY_HUB_JWKS_URL` (default GitHub's), `DEPLOY_HUB_JWKS_CACHE`, `HUB_REPO`.

`status` columns: `sha` is the sha7 of the running version; `health` is
`<container state>/<check>` where `<check>` is the container healthcheck, a live
HTTP probe (`ok`/`fail`) for HTTP apps without one, or `-`.

## MCP for agents

`deploy-hub mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server for AI agents (Claude Code, Claude Desktop, any MCP client). It speaks
newline-delimited JSON-RPC on stdin/stdout (protocol `2025-06-18`) and turns
every tool call into one runner request over the deploy channel — exactly
`ssh -i $DEPLOY_KEY -o IdentitiesOnly=yes -o BatchMode=yes deploy@$VPS_ADDR "<request>"`,
the same channel as `onboard status`. Run it on the operator machine; nothing
new runs on the server.

| Tool | Arguments | Runner request | Kind |
|---|---|---|---|
| `status` | — | `status` | read-only |
| `history` | `app` | `history <app>` | read-only |
| `rollback` | `app`, optional `tag` (`sha-<7..40 hex>`) | `rollback <app> [tag]` | destructive: restarts the app |

There is no `deploy` or `report` tool: those need a GitHub OIDC token that only
the hub's workflow has, so an agent ships new code the normal way — a push to
the app repo. `rollback` is bound by the runner's rules: only versions this hub
deployed successfully before, journaled as `rollback`/`redeploy`.
Arguments are validated before anything is sent (app name and tag regex); an
invalid argument, a refusal or a failed health gate comes back as a tool result
with `isError: true` and the runner's own output, so the agent sees why.

Settings (environment of the MCP process): `VPS_ADDR` (required), `DEPLOY_KEY`
(default `~/.ssh/deploy_hub_key`), `DEPLOY_USER` (default `deploy`). The host
key must already be in `known_hosts` (`BatchMode` never prompts). Logs go to
stderr as JSON (`DEPLOY_HUB_LOG_LEVEL`, default `warn`; `info` logs every
tool call).

Connect it to Claude Code (build the binary for the operator machine first, see
[The binary](#the-binary)):

```
claude mcp add deploy-hub --env VPS_ADDR=<vps> -- /path/to/deploy-hub mcp
```

or in a project's `.mcp.json` / Claude Desktop's config:

```json
{
  "mcpServers": {
    "deploy-hub": {
      "command": "/path/to/deploy-hub",
      "args": ["mcp"],
      "env": {"VPS_ADDR": "<vps>", "DEPLOY_KEY": "/Users/<you>/.ssh/deploy_hub_key"}
    }
  }
}
```

Check it by hand without a client:

```
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{}}}' \
  | VPS_ADDR=<vps> deploy-hub mcp
```

Agents that should only watch can be denied the `rollback` tool in the client
(in Claude Code: `"permissions": {"deny": ["mcp__deploy-hub__rollback"]}`).

## Health gate and rollback

The gate polls every 5 s for up to 90 s:

- `static`/`service` (or BYOC `hmode=http`) — HTTP 2xx/3xx from
  `127.0.0.1:<port><health_path>`;
- `bot` (or BYOC `hmode=compose`) — the container's own healthcheck reports
  `healthy`; make it check real work, not just "process is up";
- BYOC `hmode=process` — the main container is still running after one tick.

Blue-green, for every app whose old instance is running: the new image starts
as a standby `<app>-bg` with `docker compose run --no-deps` of the main
service, so it gets the service's command, env, volumes, networks,
healthcheck and limits (not its ports or restart policy), and is gated there.
Only a healthy standby lets the real service be recreated and gated; a broken
release never touches the live container.

Apps whose live container has a writable mount are recreated in place instead:
the standby would share that mount with the old instance, so two processes
would write the same state at once (tasks: SQLite in `/data`). Such apps take
a few seconds of 502, and a failed gate rolls back to the previous sha.
`bluegreen=on` in `app.conf` keeps the standby for an app whose mount is only a
cache; `bluegreen=off` turns it off for any app.

- App with a port: the standby publishes `127.0.0.1:<port+10000>`. While the
  real service is down Caddy sends its requests to the standby by itself (see
  HTTPS routing), so the cutover needs no Caddy reload. The standby stays up
  6 s after the real service is healthy again, until Caddy is back on the
  permanent port. When `port+10000` is past 65535, is another app's port or
  something listens on it, the app gets the first free spare port in
  30000–32767 instead; caddy-sync writes that route at the start of the
  deploy, before the standby exists. Only when no spare port is free either
  does the deploy recreate in place.
- App without a port (bots, workers, BYOC without a published port): the
  standby runs next to the old instance. Both run while the standby is gated
  and the main service is recreated — usually 5–90 s. A Telegram bot that
  long-polls gets `409 Conflict` from `getUpdates` in one of the two
  instances during that window; libraries retry it. An update the old
  instance took but did not confirm yet can reach the new one too, so make
  handlers idempotent where a duplicate matters.

After the cutover the standby is stopped gracefully (`docker stop`, so it gets
its `stop_grace_period`, at most 2 minutes, to finish what it took over) and
then removed; on
every other exit path, including SIGINT/SIGTERM/SIGHUP, it is removed at once.
The old main container is stopped the same way during the recreate: generated
composes set `stop_grace_period: 30s` (BYOC: only where you did not set one),
so an app that drains on SIGTERM finishes its in-flight requests and jobs.
Apps provisioned before this default keep Docker's 10 s until the line is
added to `/opt/<app>/docker-compose.yml`.

Opt out of running two instances with the label `deploy-hub.overlap: "false"`
on the main service in the compose (or `LABEL deploy-hub.overlap=false` in the
image; the compose label wins when both are set): the deploy then recreates
in place, with downtime, for apps that must
never run twice (an exclusive lock, a singleton consumer). When the old instance was not running, a failed gate
redeploys the previous sha; on a first deploy the app is stopped.

## Observability

Logs — every server command writes JSON lines to
`/opt/deploy-hub/logs/<command>.log`; `gate` and `tools` log JSON to stderr in
CI (each check, its values and findings; mutation progress). Every command logs
`command started` and `command finished` (exit code, result, duration, number
of warning and error records). A panic in a command or in one gate check is
recovered and logged with its stack (`command panicked`, `gate check
panicked`); the command exits 1, the crashed check fails with the value
`crashed=1` and the other checks still run. To ship the server
logs to VictoriaLogs, the observability stack's Vector needs a `file` source
over `/opt/deploy-hub/logs/*.log`. Secrets (registry token, OIDC token, deploy
key, `.env` content) are never logged.

A CI step that fails before the gate wrote a verdict (checkout, toolchain,
build inputs, image build, or the gate itself crashing or timing out) is still
reported: the deploy workflow sends a one-check verdict named `ci-<step>`, so
the journal records `gate fail`, the tracker gets `checks fail` and the
metrics show which step broke.

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
| `deploy_hub_identity_verified` | app, verb | 1 when the OIDC identity of a deploy/report/mutation-cache request verified, 0 when refused |
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
| `deploy_hub_host_sync_success`, `deploy_hub_host_files_changed` | app | 1/0 and the number of replaced files for every `host-sync` run |
| `deploy_hub_self_update_success`, `deploy_hub_self_update_installed` | — | 1/0 for every `self-update` run, and whether it installed a new binary |
| `deploy_hub_command_exit_code` | command | exit code of a server command run (`runner`, `caddy-sync`, `host-sync`, `prune`, `provision`, `facts`); one sample per run |
| `deploy_hub_command_duration_seconds` | command, result | run time; result `ok`, `fail`, `usage` or `panic` |
| `deploy_hub_command_panicked` | command | 1 when the run recovered a panic |
| `deploy_hub_command_log_records` | command, level | warning and error log records of the run |
| `deploy_hub_request` | verb, app | one sample per runner request for a registered app (`app="-"` for `status`) |
| `deploy_hub_request_refused` | verb, reason | one sample per refusal; reason `bad-request`, `unknown-app`, `bad-tag`, `bad-compose`, `no-identity`, `identity-unverified`, `no-repo-binding`, `not-gated`, `bad-digest`, `bad-verdict`, `bad-cache` |
| `deploy_hub_mutation_cache_intact` | app | 1 when the stored mutation cache matched its recorded sha256 on read, 0 when it did not |
| `deploy_hub_delivery_success` | — | 1/0 for the tracker post of the run (absent when delivery is off or there was nothing to post) |
| `deploy_hub_journal_readable` | — | 0 when `deploys.log` could not be read; the journal counters are then not pushed |

### Alerts

[`server/vmalert/deploy-hub.yml`](server/vmalert/deploy-hub.yml) holds the
vmalert rules over these metrics: a hub that pushed nothing for 5 minutes, a
panic, error log records, a failed server command, a failed deploy, redeploy or
rollback, a gate rejection (per check, `ci-<step>` included), a crashed gate
check, any refused request, an app unhealthy for 5 minutes, a failed Caddy
sync, a failed host-sync, a self-update failing for 40 minutes, a deploy waiting over 120 s for the lock, a failed tracker post and an
unreadable journal, a self-hosted CI runner offline for 10 minutes, its
state not pushed, broken CI isolation, and the CI disk over 85%. `go test ./internal/obs` fails when a rule uses a metric
that the code does not declare, or when a problem metric has no rule.

To load them, copy the file into the observability stack's `rules/` directory
and add `-rule=/etc/vmalert/deploy-hub.yml` to its `vmalert-metrics`
service. Check a change before shipping it:

```
docker run --rm -v $PWD/server/vmalert:/r:ro victoriametrics/vmalert:v1.152.0 -dryRun -rule=/r/deploy-hub.yml
```

### Usage analytics

Event metrics (`deploy_hub_request`, `deploy_hub_request_refused`, the gate
gauges, `deploy_hub_command_*`) get one sample per event, so counting raw
samples over a window counts events. Queries for VictoriaMetrics:

| Question | Query |
|---|---|
| Runner requests by verb and app, last 30 days | `sum by (verb, app) (count_over_time(deploy_hub_request[30d]))` |
| Refusals by reason | `sum by (reason) (count_over_time(deploy_hub_request_refused[30d]))` |
| Gate runs per app | `count_over_time(deploy_hub_gate_passed[30d])` |
| Which checks block deploys most | `sort_desc(sum by (check) (count_over_time(deploy_hub_gate_check_passed[30d]) - sum_over_time(deploy_hub_gate_check_passed[30d])))` |
| Slowest checks | `topk(5, max by (check) (max_over_time(deploy_hub_gate_check_duration_seconds[30d])))` |
| Coverage, mutation score, duplication trend | `deploy_hub_gate_check_value{check="mutation"}` (and `line-coverage`, `branch-coverage`, `duplication`) |
| CI failures before the gate | `sum by (check) (count_over_time(deploy_hub_gate_check_passed{check=~"ci-.*"}[30d]))` |
| Failed deploys and rollbacks | `sum by (app, action) (increase_prometheus(deploy_hub_operations_total{result="fail"}[30d]))` |
| Server command runs and failures | `sum by (command, result) (count_over_time(deploy_hub_command_duration_seconds[30d]))` |
| Error log records per command | `sum by (command) (sum_over_time(deploy_hub_command_log_records{level="error"}[30d]))` |

Per-app HTTP traffic comes from the Caddy access log in VictoriaLogs
(`log_type:access host:<app host>`), not from these metrics.

## HTTPS routing

Every app with a port gets `https://<app>.<host-slug>.sslip.io` →
`reverse_proxy 127.0.0.1:<port> 127.0.0.1:<standby port>` with an automatic
Let's Encrypt certificate (sslip.io resolves the name back to the VPS).
`deploy-hub caddy-sync` renders `/etc/caddy/Caddyfile` from `apps.list`,
validates it with `caddy validate`, writes it only when it changed (mode 600,
owner `caddy`) and then reloads Caddy. It runs after onboarding, after a BYOC
deploy and at the start of a blue-green deploy, before the standby exists; an
unchanged file means no reload. Settings: `hub.conf`, `CADDYFILE`,
`HOST_SLUG`, `ACME_EMAIL`, `CADDY_USER`.

The second upstream is the blue-green standby port; nothing listens there
outside a deploy. It is `port+10000`, or the first free spare port in
30000–32767 when that is past 65535, is another app's port or another
process already listens on it, so Caddy never fails over to something that is
not the app. Only when no spare port is free is it left out. `lb_policy first` keeps every request on the
permanent port while it answers. A request that fails there (`fail_duration
5s`, `max_fails 1`) takes the port out of rotation for 5 s and goes to the
standby. The proxy
retries (`lb_try_duration 30s`, `lb_try_interval 250ms`), so while a container
is recreated, requests wait for one of the two instead of getting an
instant 502. A failed connection is always retried; a request that already
reached an instance and broke is retried only for GET. The old main container
gets `stop_grace_period` (30 s by default) after SIGTERM, so a POST that is in
flight when it stops completes if the app drains on SIGTERM; an app that
exits at once on SIGTERM can still fail that POST with a 502.

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
  The runner never invokes a shell; it executes only `docker`,
  `sudo -n deploy-hub caddy-sync` and `sudo -n deploy-hub host-sync <app> <tag>`,
  from a fixed table of literal executables.
- Root on the host is reached only through the commands under `hub:` in the
  policy (`caddy-sync`, `host-sync`, `self-update`), written to sudoers by the
  binary itself. `host-sync`
  installs nothing but files of an image that passed the gate (journal) and was
  pulled by digest, to the explicit per-app list of paths, owners and modes in
  this repo's policy; see [Host files](#host-files-host-sync). The `deploy` user
  is in the `docker` group, so this narrows what the runner does by design, not
  what a full compromise of that user could do.
- `deploy-hub mcp` holds no extra power: it uses the deploy key, so an agent
  gets exactly the runner's verbs minus `deploy`/`report` (no OIDC token), and
  validates arguments before they reach SSH.
- App names: strict regex + allowlist; tags and digests: strict regex; BYOC
  metadata and every provision argument are validated on the server again.
- Containers bind to 127.0.0.1 only and carry mem limits and log rotation.
- No long-lived registry credentials on the server (ephemeral job token via
  stdin, logout guaranteed).
- App secrets live in `/opt/<app>/.env` (600), never in git; files written by
  the binary are 600.
- The self-hosted CI runner shares the host with production but not its
  privileges: a user `ci-<repo>` per repo without sudo or the system Docker socket, rootless
  Docker, a network namespace that reaches only the internet, sandboxed units
  and CPU/memory/disk caps (see [Self-hosted CI runner](#self-hosted-ci-runner)).
  Public repos and forks never get it.
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
5. Remove what the bash version left behind: every `deploy-hub-*` systemd
   unit except the prune service and timer (`systemctl disable --now`, then
   delete the unit files), the old `*.sh`/`*.py` files in `/opt/deploy-hub/bin`
   with their `*.env`/`*.log` files, and the repo secrets that only the old
   workflows used. Agents reach the hub through `deploy-hub mcp` instead.

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
