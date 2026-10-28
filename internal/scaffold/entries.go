package scaffold

import "fmt"

const (
	gateEntry = `  %[1]s:
    repo: %[2]s
    kind: service
    port: 8080
    health: /healthz
    cpus: "0.5"
    memory: 64m
    dast: {target: /healthz, spider: 1m, max_time: 10m}
    load: {rate: 20, duration: 30s, p95: 300ms, max_error_percent: 0, concurrency: 10, paths: [/healthz]}
    scenarios:
      - id: %[1]s-answers-carry-security-headers
        category: security
        tests:
          - internal/web.TestPagesAnswerJSONWithSecurityHeaders
      - id: %[1]s-panics-reach-bugsink
        category: business
        tests:
          - internal/web.TestPanicIsSentToBugsinkBeforeTheAnswer
`
	catalogEntry = `  %[1]s:
    owner: %[2]s
    critical: normal
    gives: [logs, metrics, errors]
    exposes:
      - site: %[1]s
      - port: 8080
    calls:
      - to: observability
        why: errors go to Bugsink through the Sentry SDK
    secrets: [SENTRY_DSN]
    config: [PORT, METRICS_PORT]
`
	targetFile = `- targets: ["%[1]s:9100"]
  labels: {app: %[1]s}
`
	nextSteps = `
Written %[1]s from the template. Next, in this order:

1. Create the private repo %[2]s and push the directory to main as the first commit.
2. In a pull request to deploy-hub, add the app to policy/gate.yaml under apps:

%[3]s
   and to policy/architecture.yaml under services:

%[4]s
3. In a pull request to the observability repo, add targets/%[5]s.yml:

%[6]s
4. After the hub release, connect the repo: deploy-hub onboard %[2]s
   It finds the compose, the port and the caller stub that is already here.
5. Put SENTRY_DSN of the app's Bugsink project into the secrets store.
`
)

func GateEntry(o Options) string {
	return fmt.Sprintf(gateEntry, o.App, o.Repo)
}

func CatalogEntry(o Options) string {
	return fmt.Sprintf(catalogEntry, o.App, o.Owner())
}

func TargetFile(o Options) string {
	return fmt.Sprintf(targetFile, o.App)
}

func NextSteps(o Options) string {
	return fmt.Sprintf(nextSteps, o.Dir, o.Repo, GateEntry(o), CatalogEntry(o), o.App, TargetFile(o))
}
