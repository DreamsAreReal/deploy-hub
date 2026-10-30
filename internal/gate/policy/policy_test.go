package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	dupTokensLine = "duplicate_min_tokens: 50"
	uiMapLine     = "map: web/appmap.json"
)

const (
	ownersLine = "owners: [Owner]"
	freeLine   = "mutation_free_gib: 10"
	pathsLine  = "paths: [/health]"
)

const localizedLine = "localized: [internal/texts/]"

const dataLine = "data: [/data, /cache]"

const (
	uiBlock     = "    ui: {map: web/appmap.json, seed: [seed], env: [DEV_LOGIN=1], chromium: /ms/chrome}\n"
	browserLine = "  browser: mcr.microsoft.com/playwright" + digest + "\n"
)

const digest = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const reviewedExpr = `"MIT OR (MIT AND MIT)"`

const reviewedKey = `"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`

const declaredLine = `"pkg:pypi/legacy": MIT`

const (
	deployWorkflow = ".github/workflows/deploy.yml"
	acceptedFile   = "file: " + deployWorkflow
	selfHosted     = "self-hosted-runner"
	medium         = "Medium"
	runsOn         = "runs-on"
)

const valid = `owners: [Owner]
thresholds:
  line_coverage: 90
  branch_coverage: 90
  mutation_score: 90
  max_cognitive_complexity: 9
  max_duplication_percent: 2
  duplicate_min_tokens: 50
limits:
  check_timeout: 30m
  long_check_timeout: 3h
  fuzz_time: 10s
  fuzz_examples: 200
  mutant_timeout_floor: 20s
  mutant_timeout_factor: 3
  findings_listed: 20
  stage_ready_timeout: 30s
  mutation_disk_gib: 20
  mutation_free_gib: 10
licenses: [MIT]
dependencies: [example.com/dep]
declared_licenses: {"pkg:pypi/legacy": MIT}
reviewed_license_texts: {"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "MIT OR (MIT AND MIT)", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": NONE}
forbidden_files: ["*.bak"]
markers: [todo]
sast_rulesets: [p/default]
images:
  semgrep: semgrep/semgrep` + digest + `
  zap: ghcr.io/zaproxy/zaproxy` + digest + `
  browser: mcr.microsoft.com/playwright` + digest + `
apps:
  hub:
    repo: Owner/hub
    kind: binary
    localized: [internal/texts/]
    allowed_files: [docs/STATE.md]
    accepted_workflow_findings:
      - rule: self-hosted-runner
        severity: Medium
        file: .github/workflows/deploy.yml
        key: runs-on
        reason: "isolated runner, see #600"
    scenarios:
      - id: refuse
        category: security
        tests: [internal/x.TestRefuse]
  web:
    repo: Owner/web
    kind: service
    port: 8080
    health: /health
    cpus: "0.5"
    memory: 256m
    env: [MODE=test]
    data: [/data, /cache]
    dast: {spider: 1m, max_time: 5m, openapi: /openapi.json}
    load: {rate: 20, duration: 30s, p95: 300ms, max_error_percent: 0, concurrency: 10, paths: [/health]}
    ui: {map: web/appmap.json, seed: [seed], env: [DEV_LOGIN=1], chromium: /ms/chrome}
  bot:
    repo: Owner/bot
    shell_utilities: busybox
    kind: worker
`

func TestParseValidPolicy(t *testing.T) {
	p, err := Parse([]byte(valid))
	testutil.NoErr(t, err)
	testutil.True(t, p.Owns("Owner") && !p.Owns("Other"), "owners")
	web, err := p.App("web")
	testutil.NoErr(t, err)
	testutil.True(t, web.HasImage() && web.HasHTTP(), "service has image and HTTP")
	testutil.Equal(t, web.Load.P95.Milliseconds(), int64(300))
	testutil.Equal(t, web.DAST.OpenAPI, "/openapi.json")
	testutil.Equal(t, web.Data, []string{"/data", "/cache"})
	hub, _ := p.App("hub")
	testutil.True(t, !hub.HasImage() && !hub.HasHTTP(), "binary has neither")
	testutil.Equal(t, hub.Accepted, []Accepted{{Rule: selfHosted, Severity: medium, File: deployWorkflow, Key: runsOn, Reason: "isolated runner, see #600"}})
	bot, _ := p.App("bot")
	testutil.True(t, bot.HasImage() && !bot.HasHTTP(), "worker has an image only")
	_, err = p.App("ghost")
	testutil.Is(t, err, ErrUnknownApp)
	testutil.Equal(t, p.Apps["bot"].Shell, ShellBusybox)
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate.yaml")
	testutil.Write(t, path, valid)
	_, err := LoadFile(path)
	testutil.NoErr(t, err)
	_, err = LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	testutil.True(t, os.IsNotExist(err), "missing file")
}

func repositoryPolicy(t *testing.T) Policy {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p, err := LoadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "policy", "gate.yaml"))
	testutil.NoErr(t, err)
	return p
}

func TestRepositoryPolicyIsValid(t *testing.T) {
	testutil.True(t, repositoryPolicy(t).Mutation.SkipInDeploys, "mutation testing runs only in the full runs")
}

func TestRepositoryPolicyAcceptsOneFinding(t *testing.T) {
	p := repositoryPolicy(t)
	accepted := map[string][]Accepted{}
	for name, app := range p.Apps {
		if len(app.Accepted) > 0 {
			accepted[name] = app.Accepted
		}
	}
	testutil.Equal(t, len(accepted["deploy-hub"]), 1)
	testutil.Equal(t, len(accepted), 1)
	only := accepted["deploy-hub"][0]
	testutil.Equal(t, [4]string{only.Rule, only.Severity, only.File, only.Key}, [4]string{selfHosted, medium, deployWorkflow, runsOn})
}

func TestParseRejects(t *testing.T) {
	cases := map[string][2]string{
		"unknown field":      {ownersLine, ownersLine + "\nextra: 1"},
		"not yaml":           {ownersLine, "owners: [Owner"},
		"no owners":          {ownersLine, "owners: []"},
		"zero coverage":      {"line_coverage: 90", "line_coverage: 0"},
		"coverage over 100":  {"branch_coverage: 90", "branch_coverage: 101"},
		"mutation":           {"mutation_score: 90", "mutation_score: -1"},
		"duplication":        {"max_duplication_percent: 2", "max_duplication_percent: 0"},
		"cognitive":          {"max_cognitive_complexity: 9", "max_cognitive_complexity: 0"},
		"tokens":             {dupTokensLine, "duplicate_min_tokens: 0"},
		"net growth":         {dupTokensLine, "duplicate_min_tokens: 50\n  max_net_growth_lines: -1"},
		"check timeout":      {"check_timeout: 30m", "check_timeout: 0s"},
		"long check timeout": {"long_check_timeout: 3h", "long_check_timeout: 0s"},
		"fuzz time":          {"fuzz_time: 10s", "fuzz_time: 0s"},
		"fuzz examples":      {"fuzz_examples: 200", "fuzz_examples: 0"},
		"mutant floor":       {"mutant_timeout_floor: 20s", "mutant_timeout_floor: 0s"},
		"mutant factor":      {"mutant_timeout_factor: 3", "mutant_timeout_factor: 0"},
		"findings listed":    {"findings_listed: 20", "findings_listed: 0"},
		"stage ready":        {"stage_ready_timeout: 30s", "stage_ready_timeout: 0s"},
		"mutation disk":      {"mutation_disk_gib: 20", "mutation_disk_gib: 0"},
		"mutation free":      {freeLine, "mutation_free_gib: 0"},
		"too many parallel":  {freeLine, freeLine + "\n  parallel_checks: 17"},
		"negative parallel":  {freeLine, freeLine + "\n  parallel_checks: -1"},
		"licenses":           {"licenses: [MIT]", "licenses: []"},
		"declared license":   {declaredLine, `"pkg:pypi/legacy": GPL-3.0-only`},
		"declared purl":      {declaredLine, `"pypi/legacy": MIT`},
		"declared version":   {declaredLine, `"pkg:pypi/legacy@1": MIT`},
		"markers":            {"markers: [todo]", "markers: []"},
		"shell utilities":    {"shell_utilities: busybox", "shell_utilities: coreutils"},
		"reviewed key":       {reviewedKey, `"md5:abc"`},
		"reviewed license":   {reviewedExpr, `"GPL-3.0-only"`},
		"reviewed trailing":  {reviewedExpr, `"MIT AND"`},
		"reviewed leading":   {reviewedExpr, `"OR MIT"`},
		"reviewed adjacent":  {reviewedExpr, `"MIT MIT"`},
		"reviewed empty":     {reviewedExpr, `""`},
		"rulesets":           {"sast_rulesets: [p/default]", "sast_rulesets: []"},
		"unpinned semgrep":   {"semgrep: semgrep/semgrep" + digest, "semgrep: semgrep/semgrep:latest"},
		"unpinned zap":       {"zap: ghcr.io/zaproxy/zaproxy" + digest, "zap: ghcr.io/zaproxy/zaproxy:stable"},
		"unpinned browser":   {"browser: mcr.microsoft.com/playwright" + digest, "browser: mcr.microsoft.com/playwright:v1"},
		"no browser":         {browserLine, ""},
		"ui map":             {uiMapLine, "map: ''"},
		"ui map outside":     {uiMapLine, "map: ../appmap.json"},
		"ui map absolute":    {uiMapLine, "map: /web/appmap.json"},
		"ui map unclean":     {uiMapLine, "map: web/../appmap.json"},
		"ui chromium":        {"chromium: /ms/chrome", "chromium: ms/chrome"},
		"app name":           {"  hub:\n", "  Hub!:\n"},
		"repo":               {"repo: Owner/hub", "repo: hub"},
		"kind":               {"kind: binary", "kind: daemon"},
		"port":               {"port: 8080", "port: 0"},
		"health":             {"health: /health", "health: health"},
		"memory":             {"memory: 256m", "memory: lots"},
		"cpus":               {`cpus: "0.5"`, `cpus: "half"`},
		"rate":               {"rate: 20", "rate: 0"},
		"duration":           {"duration: 30s", "duration: 0s"},
		"p95":                {"p95: 300ms", "p95: 0s"},
		"concurrency":        {"concurrency: 10", "concurrency: 0"},
		"load paths":         {pathsLine, "paths: []"},
		"load path value":    {pathsLine, "paths: [health]"},
		"spider":             {"spider: 1m", "spider: 0s"},
		"dast max time":      {"max_time: 5m", "max_time: 0s"},
		"openapi":            {"openapi: /openapi.json", "openapi: openapi"},
		"scenario id":        {"id: refuse", `id: ""`},
		"scenario category":  {"category: security", "category: style"},
		"scenario tests":     {"tests: [internal/x.TestRefuse]", "tests: []"},
		"empty localized":    {localizedLine, `localized: [""]`},
		"absolute localized": {localizedLine, "localized: [/etc/]"},
		"escaping localized": {localizedLine, "localized: [a/../b]"},
		"bad localized":      {localizedLine, "localized: [\"[\"]"},
		"escaping allowed":   {"allowed_files: [docs/STATE.md]", "allowed_files: [../STATE.md]"},
		"accepted rule":      {"rule: self-hosted-runner", `rule: ""`},
		"accepted severity":  {"severity: Medium", `severity: ""`},
		"accepted key":       {"key: runs-on", `key: ""`},
		"accepted no task":   {`reason: "isolated runner, see #600"`, `reason: "isolated runner"`},
		"accepted glob":      {acceptedFile, "file: .github/workflows/*.yml"},
		"accepted escape":    {acceptedFile, "file: ../deploy.yml"},
		"accepted no file":   {acceptedFile, `file: ""`},
		"relative data":      {dataLine, "data: [data]"},
		"root data":          {dataLine, "data: [/]"},
		"unclean data":       {dataLine, "data: [/data/]"},
		"data with comma":    {dataLine, "data: [\"/a,b\"]"},
		"repeated data":      {dataLine, "data: [/data, /data]"},
		"duplicate scenario": {"        tests: [internal/x.TestRefuse]\n", "        tests: [internal/x.TestRefuse]\n      - id: refuse\n        category: business\n        tests: [a.TestB]\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.True(t, strings.Contains(valid, c[0]), "fixture has "+c[0])
			_, err := Parse([]byte(strings.Replace(valid, c[0], c[1], 1)))
			testutil.Fails(t, err)
		})
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte("owners: []"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err == nil && (len(p.Owners) == 0 || p.Thresholds.MutationScore <= 0 || p.Limits.Check <= 0) {
			t.Fatalf("invalid policy accepted: %+v", p)
		}
	})
}

func TestOnlyAnAppWithAnInterfaceNeedsTheBrowserImage(t *testing.T) {
	plain := strings.Replace(strings.Replace(valid, uiBlock, "", 1), browserLine, "", 1)
	testutil.True(t, plain != valid && !strings.Contains(plain, "browser:"), "fixture lost the ui block and the browser")
	p, err := Parse([]byte(plain))
	testutil.NoErr(t, err)
	testutil.True(t, p.Apps["web"].UI == nil, "no ui")
	withUI, err := Parse([]byte(valid))
	testutil.NoErr(t, err)
	testutil.Equal(t, withUI.Apps["web"].UI.Chromium, "/ms/chrome")
	testutil.Equal(t, withUI.Apps["web"].UI.Map, "web/appmap.json")
}

func TestIsLocalized(t *testing.T) {
	app := App{Localized: []string{"internal/texts/", "*_test.go", "web/static/*.json"}}
	cases := map[string]bool{
		"internal/texts/a.go":   true,
		"x/internal/texts/a.go": true,
		"internal/textsx/a.go":  false,
		"a/internal/texts.go":   false,
		"a_test.go":             true,
		"dir/a_test.go":         true,
		"a_test.go.txt":         false,
		"web/static/m.json":     true,
		"web/static/sub/m.json": false,
		"x/web/static/m.json":   false,
		"README.md":             false,
	}
	for rel, want := range cases {
		testutil.Equal(t, app.IsLocalized(rel), want)
	}
	testutil.Equal(t, App{Localized: []string{"texts/"}}.IsLocalized("atexts/a.go"), false)
	testutil.Equal(t, App{}.IsLocalized("internal/texts/a.go"), false)
}

func TestAllowsFile(t *testing.T) {
	app := App{Allowed: []string{"docs/STATE.md", "docs/tasks/*/STATE.md"}}
	testutil.True(t, app.AllowsFile("docs/STATE.md"), "listed path")
	testutil.True(t, app.AllowsFile("docs/tasks/web-app/STATE.md"), "pattern path")
	testutil.True(t, !app.AllowsFile("STATE.md"), "other directory")
	testutil.True(t, !app.AllowsFile("docs/tasks/a/b/STATE.md"), "deeper path")
	testutil.True(t, !app.AllowsFile("docs/.env"), "unlisted file in docs")
	testutil.True(t, !App{}.AllowsFile("docs/STATE.md"), "nothing allowed by default")
}
