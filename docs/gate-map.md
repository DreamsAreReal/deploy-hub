# What the gate catches, and what it does not

Agents change the apps without anyone reading the diffs (30 days: 1182 unattended Claude
sessions, about 16 interactive ones), so the gate is the review. This map lists what an
agent can break, the check that stops it, and the gaps. Checks are only ever added;
none of them is weakened to make a release pass.

| An agent could… | Caught by | Status |
|---|---|---|
| ship code that does not build or fails a test | `tests`, per stack | yes |
| ship untested code or tests that assert nothing | `line-coverage`, `branch-coverage`, `mutation` (90%) | yes |
| break a security or business scenario | `scenarios` (registered tests must pass) | yes |
| commit a secret | `secrets-gitleaks`, `secrets-trufflehog` (whole history), `hygiene` (key files) | yes |
| add a vulnerable or unknown dependency | `dependencies` (approved list), `sca`, `vulnerabilities`, `sbom-licenses` | yes |
| hide a finding with a suppression comment | `comments` (no comments at all), `hygiene` (no suppression files) | yes |
| leave work half done | `markers` | yes |
| make code unreadable or copy-pasted | `lint`, `cognitive-complexity` (9), `duplication` (2%), `dead-code` | yes |
| open a hole in CI or the image | `workflows`, `dockerfile`, `iac`, `image` | yes |
| break the running app's security or speed | `dast`, `load` | yes |
| add a link to another service, a port or an env variable nobody declared | `architecture` against `policy/architecture.yaml` | yes |
| make an app call tasks directly | `architecture` (`tasks_callers`, dated exceptions) | yes |
| put a secret outside the store | `architecture` (an env variable must be a declared secret or config) + deploy-hub lays only stored keys | partly: a declared secret may still be set by hand on the server until the app moves to the store (#743) |
| weaken the gate | thresholds and exceptions live only in this repository's `policy/`; app repositories cannot change them | yes, as long as this repository needs the owner's review (#812) |
| ship a broken screen or button | `ui` (the hub's crawler on the seeded stage, driven by the app map: errors, dead or covered buttons, delete without confirmation) | yes for apps with `ui` in the policy; a full gate run presses every button |
| make the interface unreadable, cramped or confusing | `ui` (cut text or text out of its box, page wider than the screen, contrast, duplicate or ambiguous buttons, near-aligned edges, cramped controls; every screen at 360 px too, five sizes and the dark theme in a full run) | yes for apps with `ui`; text quality and taste are not checked |
| change UI markup or look by accident | tasks' own screen snapshots and design rules | partly: per app (#808); no screenshot comparison yet (#809) |
| add a cron job, timer or a new public site | `architecture`: a scheduled workflow or a host timer must be in the entry's `timers` with a `why`, a host caddy file must be a `site` in `exposes`; `deploy_hub_arch_live_drift`: the public site the hub serves for an app; `deploy_hub_arch_host_drift`: every timer and caddy file on the server, whoever installed it | yes; a loop inside the app's own process is its code, covered by tests, not by the catalog |
| grow code without removing any (month: +76.7k / −4.7k lines) | `growth`: lines added, removed and net over the last 30 days, per app and release, as gate values | report only: `max_net_growth_lines` is 0 until the owner picks a limit; since 20.10 tasks main gained +60.7k / −9.0k |
| break API or database compatibility so a rollback fails | `rollback`: the deployed release must start and answer the `load` paths without a 5xx on the data this release seeded and migrated in the stage | yes for apps with `data`; what other services call is not compared |
| change behaviour without updating the docs | — | **no** (#822: touched modules vs touched docs) |
| hard-code domain logic into the assistant | — | **no** (#822: heuristics on assistant code) |
| deploy without the gate | deploy credentials are used only by the hub workflow after the gate; the deploy key and the OIDC token request are gone while the app's code runs, and only the image built before the gate is pushed | partly: a process the app's code leaves behind shares the job (#812: deploy in its own job), #814: one deploy key per app |
| replace the hub binary | self-update installs a release only when the binary is byte for byte the artifact its green `ci.yml` run on `main` built; a release token cannot change run artifacts | yes (#813) |

Speed matters as much: the tasks gate takes 3.3–5.4 h today. New checks run on what
changed and in parallel with the heavy ones; a check that slows every release has to
pay for itself.
