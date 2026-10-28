package arch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	envExample  = ".env.example"
	appName     = "app"
	appRepo     = "owner/app"
	siblingName = "sibling"
	normal      = "normal"
	envExampleA = "PORT=8080\nAPI_TOKEN=\n# a note\nnot a pair\n"
	composeBody = "services:\n  web:\n    ports: [\"127.0.0.1:9000:8080\", \"9100/tcp\", \"x:y\"]\n    environment:\n      MODE: prod\n  worker:\n    environment: [\"QUEUE=jobs\", \"DEBUG\"]\n"
	cleanEntry  = "services:\n  app:\n    critical: normal\n    gives: [logs]\n    exposes: [{port: 8080}, {port: 9100}]\n    secrets: [API_TOKEN]\n    config: [PORT, MODE, QUEUE, DEBUG, EXTRA]\n"
)

var day = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func mustParse(t *testing.T, body string) Catalog {
	t.Helper()
	catalog, err := Parse([]byte(body))
	testutil.NoErr(t, err)
	return catalog
}

func repoWith(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	testutil.Write(t, filepath.Join(src, envExample), envExampleA)
	testutil.Write(t, filepath.Join(src, "worker.env.example"), "QUEUE=jobs\nEXTRA=1\n")
	testutil.Write(t, filepath.Join(src, composeFiles[0]), composeBody)
	return src
}

func workspace(t *testing.T, catalog, src string) gate.Workspace {
	t.Helper()
	dir := t.TempDir()
	if catalog != "" {
		testutil.Write(t, filepath.Join(dir, CatalogFile), catalog)
	}
	return gate.Workspace{Src: src, PolicyDir: dir, AppName: appName, Repo: appRepo,
		Policy: policy.Policy{Apps: map[string]policy.App{appName: {Repo: appRepo}, siblingName: {Repo: appRepo}, "other": {Repo: "owner/other"}}}}
}

func TestTheShippedCatalogIsValid(t *testing.T) {
	catalog, err := Load(filepath.Join("..", "..", "..", "policy"))
	testutil.NoErr(t, err)
	for name := range catalog.Services {
		if problems := catalog.Problems(name, day); len(problems) != 0 {
			t.Errorf("%s: %v", name, problems)
		}
	}
	testutil.Contains(t, catalog.Mermaid(), "graph LR", "router_bot[router-bot] -->|", "(until 2026-12-31, #785)")
}

func TestParseRefusesUnknownFieldsAndEmptyCatalogs(t *testing.T) {
	for _, body := range []string{"services: {}\n", "tasks_callers: []\n", "services:\n  app:\n    colour: red\n", "services: ["} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("a missing catalog was loaded")
	}
}

func TestProblemsOfAnEntry(t *testing.T) {
	const head = "services:\n  tasks: {critical: high}\n  app:\n    critical: normal\n"
	cases := map[string]string{
		"critical must be one of":         "services:\n  app:\n    critical: vital\n",
		`gives "noise" is not one of`:     head + "    gives: [noise]\n",
		`calls "ghost", which is not`:     head + "    calls: [{to: ghost, why: x}]\n",
		"without a why":                   head + "    calls: [{to: app}]\n",
		"only tasks_callers may":          head + "    calls: [{to: tasks, why: x}]\n",
		`until "soon" is not a date`:      head + "    calls: [{to: tasks, why: x, until: soon, task: \"#1\"}]\n",
		"ended on 2026-10-26 (#7)":        head + "    calls: [{to: tasks, why: x, until: \"2026-10-26\", task: \"#7\"}]\n",
		"<pass with a running exception>": head + "    calls: [{to: tasks, why: x, until: \"2026-10-28\", task: \"#7\"}]\n",
		"<pass as an allowed caller>":     "tasks_callers: [app]\n" + head + "    calls: [{to: tasks, why: x}]\n",
	}
	for want, body := range cases {
		problems := strings.Join(mustParse(t, body).Problems(appName, day), "; ")
		if strings.HasPrefix(want, "<pass") {
			if problems != "" {
				t.Errorf("%s: %s", want, problems)
			}
			continue
		}
		if !strings.Contains(problems, want) {
			t.Errorf("want %q, got %q", want, problems)
		}
	}
}

func TestGatherReadsEnvExamplesAndCompose(t *testing.T) {
	facts, err := Gather(repoWith(t))
	testutil.NoErr(t, err)
	testutil.Equal(t, strings.Join(facts.Env, ","), "PORT,API_TOKEN,QUEUE,EXTRA,MODE,DEBUG")
	testutil.Equal(t, facts.Ports, []int{8080, 9100})
	empty, err := Gather(t.TempDir())
	testutil.NoErr(t, err)
	testutil.Equal(t, len(empty.Env)+len(empty.Ports), 0)
}

func TestGatherReportsBrokenCompose(t *testing.T) {
	src := t.TempDir()
	testutil.Write(t, filepath.Join(src, composeFiles[1]), "services: [")
	if _, err := Gather(src); err == nil {
		t.Fatal("broken compose accepted")
	}
	testutil.Mkdir(t, filepath.Join(t.TempDir(), "x"))
	unreadable := t.TempDir()
	testutil.Mkdir(t, filepath.Join(unreadable, composeFiles[2]))
	if _, err := Gather(unreadable); err == nil {
		t.Fatal("a folder named like compose was read")
	}
	folder := t.TempDir()
	testutil.Mkdir(t, filepath.Join(folder, envExample))
	if _, err := Gather(folder); err == nil {
		t.Fatal("a folder named like .env.example was read")
	}
	if _, err := Gather("["); err == nil {
		t.Fatal("bad glob accepted")
	}
}

func TestCheckComparesTheRepoWithItsEntry(t *testing.T) {
	src := repoWith(t)
	outcome := Check(t.Context(), workspace(t, cleanEntry, src))
	testutil.Equal(t, outcome.Status, gate.StatusPass)
	drifting := strings.Replace(strings.Replace(cleanEntry, ", {port: 9100}", "", 1), "secrets: [API_TOKEN]", "secrets: []", 1)
	outcome = Check(t.Context(), workspace(t, drifting, src))
	testutil.Equal(t, outcome.Status, gate.StatusFail)
	testutil.Contains(t, strings.Join(outcome.Findings, "; "), "environment variable API_TOKEN", "port 9100 is published")
	split := "services:\n  app:\n    critical: normal\n    exposes: [{port: 8080}, {port: 9100}]\n    config: [PORT, MODE, QUEUE, DEBUG, EXTRA]\n  sibling:\n    critical: low\n    secrets: [API_TOKEN]\n"
	testutil.Equal(t, Check(t.Context(), workspace(t, split, src)).Status, gate.StatusPass)
}

func TestCheckFailsWithoutCatalogOrEntry(t *testing.T) {
	src := repoWith(t)
	for _, catalog := range []string{"", "services:\n  other: {critical: low}\n"} {
		if outcome := Check(t.Context(), workspace(t, catalog, src)); outcome.Status != gate.StatusFail {
			t.Errorf("%q: %+v", catalog, outcome)
		}
	}
	broken := t.TempDir()
	testutil.Write(t, filepath.Join(broken, composeFiles[0]), "services: [")
	if outcome := Check(t.Context(), workspace(t, cleanEntry, broken)); outcome.Status != gate.StatusFail {
		t.Errorf("broken compose: %+v", outcome)
	}
}

func TestMermaidIsStable(t *testing.T) {
	catalog := mustParse(t, "services:\n  b-two:\n    critical: low\n    calls: [{to: a, why: x|y, until: \"2026-12-31\", task: \"#9\"}]\n  a:\n    critical: low\n    calls: [{to: b-two, why: back}]\n")
	want := []string{"graph LR", "  a[a] -->|back| b_two[b-two]", "  b_two[b-two] -->|x/y (until 2026-12-31, #9)| a[a]"}
	testutil.Equal(t, catalog.Mermaid(), strings.Join(want, "\n")+"\n")
	testutil.Equal(t, normal, catalog.Services["a"].Critical[:0]+normal)
}
