package pack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	stackA    = "alpha"
	stackB    = "beta"
	stackC    = "gamma"
	checkName = "probe"
	webRoot   = "web"
	manifestA = "a.mod"
	readme    = "README.md"
	testGo    = "go.TestA"
	testWeb   = "Web.Tests.B"
)

var errSim = errors.New("simulated")

func testPacks(run Run) []Pack {
	return []Pack{
		{Stack: stackA, Manifests: []string{manifestA}, Sources: []string{".go"}, Checks: map[string]Run{checkName: run}},
		{Stack: stackB, Manifests: []string{"*.proj"}, Sources: []string{".cs"}, Checks: map[string]Run{checkName: run}},
		{Stack: stackC, Sources: []string{".sh"}, Loose: true, Checks: map[string]Run{checkName: run}},
	}
}

func TestDiscoverFindsProjectsAndOrphans(t *testing.T) {
	files := []string{
		manifestA, "main.go", "pkg/x.go",
		"web/app.proj", "web/src/a.cs", "web/tests/t.proj", "web/tests/t.cs", "lonely.cs",
		"scripts/run.sh", "Gemfile", "lib/tool.rb", readme,
	}
	layout := Discover(files, testPacks(nil))
	labels := make([]string, 0, len(layout.Projects))
	for _, p := range layout.Projects {
		labels = append(labels, p.Label())
	}
	testutil.Equal(t, labels, []string{stackA, stackB + ":web", stackB + ":web/tests", stackC})
	testutil.Equal(t, layout.Orphans, []string{
		"lib/tool.rb: no language pack for .rb files",
		"lonely.cs: beta source outside a project (*.proj)",
	})
}

func TestLooseSourcesJoinARootProject(t *testing.T) {
	layout := Discover([]string{"a.sh", "deep/b.sh"}, testPacks(nil))
	testutil.Equal(t, len(layout.Projects), 1)
	testutil.Equal(t, layout.Projects[0].Root, rootDir)
	none := Discover([]string{readme}, testPacks(nil))
	testutil.Equal(t, len(none.Projects)+len(none.Orphans), 0)
	rooted := Discover([]string{manifestA, "x.go", "sub/a.mod", "sub/y.go", "subway/z.go"}, testPacks(nil))
	testutil.Equal(t, len(rooted.Projects), 2)
	testutil.Equal(t, len(rooted.Orphans), 0)
	nested := Discover([]string{"svc/a.mod", "svc/x.go", "svcx/y.go"}, testPacks(nil))
	testutil.Equal(t, nested.Orphans, []string{"svcx/y.go: alpha source outside a project (a.mod)"})
	testutil.Equal(t, nearestRoot([]string{".", webRoot, "web/sub"}, "web/sub/deep"), "web/sub")
	testutil.Equal(t, nearestRoot([]string{webRoot}, "webapp"), "")
	testutil.Equal(t, nearestRoot([]string{webRoot}, webRoot), webRoot)
}

func TestOutermostRootsAbsorbNestedManifests(t *testing.T) {
	packs := testPacks(nil)
	packs[1].Outermost = true
	layout := Discover([]string{"app.proj", "src/a/a.proj", "src/a/x.cs", "tests/t/t.proj", "tests/t/y.cs"}, packs)
	testutil.Equal(t, len(layout.Projects), 1)
	testutil.Equal(t, layout.Projects[0].Root, rootDir)
	split := Discover([]string{"svc/a.proj", "svc/x.cs", "web/b.proj", "web/sub/c.proj", "web/y.cs"}, packs)
	testutil.Equal(t, len(split.Projects), 2)
	testutil.Equal(t, outermost([]string{".", "a"}), []string{"."})
	nested := "ab"
	testutil.Equal(t, outermost([]string{"a", "a/b", nested}), []string{"a", nested})
}

func TestProjectWorkspace(t *testing.T) {
	ws := gate.Workspace{Src: "/src", Out: t.TempDir()}
	p := Project{Pack: &Pack{Stack: stackB}, Root: "web/api"}
	pws, err := p.Workspace(ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, pws.Src, filepath.Join("/src", "web", "api"))
	testutil.Equal(t, pws.Out, filepath.Join(ws.Out, "beta-web-api"))
	info, err := os.Stat(pws.Out)
	testutil.True(t, err == nil && info.IsDir(), "out dir created")
	root, err := (Project{Pack: &Pack{Stack: stackA}, Root: rootDir}).Workspace(ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, root.Out, filepath.Join(ws.Out, stackA))
	blocked := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocked, "")
	_, err = p.Workspace(gate.Workspace{Out: blocked})
	testutil.Fails(t, err)
}

func repoWS(t *testing.T, files ...string) gate.Workspace {
	t.Helper()
	listing := strings.Join(files, "\x00") + "\x00"
	return gate.Workspace{Src: t.TempDir(), Out: t.TempDir(), Log: slog.New(slog.DiscardHandler), Exec: &procexectest.Fake{
		Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stdout: listing}, nil },
	}}
}

func TestDispatchRunsEveryProject(t *testing.T) {
	var seen []string
	run := func(_ context.Context, ws gate.Workspace) gate.Outcome {
		seen = append(seen, filepath.Base(ws.Src)+"|"+filepath.Base(ws.Out))
		if strings.HasSuffix(ws.Out, "web") {
			return gate.Fail("broken", "f1")
		}
		return gate.Pass(map[string]float64{"v": 1})
	}
	ws := repoWS(t, manifestA, "x.go", "web/w.proj", "web/w.cs")
	out := Dispatch(testPacks(run), checkName)(context.Background(), ws)
	testutil.Equal(t, seen, []string{filepath.Base(ws.Src) + "|" + stackA, "web|beta-web"})
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, "beta:web: broken")
	testutil.Equal(t, out.Findings, []string{"beta:web: f1"})
	testutil.Equal(t, out.Values, map[string]float64{"alpha.v": 1})
	single := Dispatch(testPacks(run), checkName)(context.Background(), repoWS(t, manifestA, "x.go"))
	testutil.Equal(t, single, gate.Pass(map[string]float64{"v": 1}))
	empty := Dispatch(testPacks(run), checkName)(context.Background(), repoWS(t, readme))
	testutil.Equal(t, empty.Status, gate.StatusNA)
	testutil.Equal(t, empty.Reason, reasonNoCode)
	broken := repoWS(t)
	broken.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	testutil.Contains(t, Dispatch(testPacks(run), checkName)(context.Background(), broken).Reason, "git ls-files")
	blockedOut := repoWS(t, manifestA, "x.go")
	blockedOut.Out = filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blockedOut.Out, "")
	testutil.Equal(t, Dispatch(testPacks(run), checkName)(context.Background(), blockedOut).Status, gate.StatusFail)
}

func TestMergeKeepsTheWorstStatus(t *testing.T) {
	na := Labeled{Label: "x", Outcome: gate.Outcome{Status: gate.StatusNA, Reason: "nothing to run"}}
	pass := Labeled{Label: "y", Outcome: gate.Pass(nil)}
	testutil.Equal(t, Merge([]Labeled{na, pass}).Status, gate.StatusPass)
	testutil.Equal(t, Merge([]Labeled{na, pass}).Reason, "")
	both := Merge([]Labeled{na, {Label: "z", Outcome: gate.Outcome{Status: gate.StatusNA, Reason: "empty"}}})
	testutil.Equal(t, both.Status, gate.StatusNA)
	testutil.Equal(t, both.Reason, "x: nothing to run; z: empty")
	testutil.Equal(t, worst([]Labeled{pass, {Outcome: gate.Fail("a")}, na}), gate.StatusFail)
	testutil.Equal(t, worst(nil), gate.StatusNA)
}

func TestStacksCheck(t *testing.T) {
	out := Stacks(testPacks(nil))(context.Background(), repoWS(t, manifestA, "x.go", "run.sh", "tool.rb"))
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, reasonOrphans)
	testutil.Equal(t, out.Values, map[string]float64{ValueProjects: 2, stackA: 1, stackC: 1})
	testutil.Equal(t, Stacks(testPacks(nil))(context.Background(), repoWS(t, manifestA, "x.go")).Status, gate.StatusPass)
	broken := repoWS(t)
	broken.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	testutil.Equal(t, Stacks(testPacks(nil))(context.Background(), broken).Status, gate.StatusFail)
}

func TestScenariosUseEveryProject(t *testing.T) {
	packs := testPacks(nil)
	packs[0].Passed = func(context.Context, gate.Workspace) (map[string]bool, error) {
		return map[string]bool{testGo: true}, nil
	}
	packs[1].Passed = func(context.Context, gate.Workspace) (map[string]bool, error) {
		return map[string]bool{testWeb: true}, nil
	}
	ws := repoWS(t, manifestA, "x.go", "web/w.proj", "web/w.cs")
	ws.App.Scenarios = []policy.Scenario{
		{ID: "one", Category: policy.CategorySecurity, Tests: []string{testGo, testWeb}},
		{ID: "two", Category: policy.CategorySecurity, Tests: []string{"go.TestMissing"}},
		{ID: "three", Category: policy.CategoryBusiness, Tests: []string{testWeb}},
	}
	out := Scenarios(packs)(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{"two (security): test go.TestMissing did not pass"})
	testutil.Equal(t, out.Values, map[string]float64{ValueSecurityScenarios: 50, ValueBusinessScenarios: 100, ValueScenarios: 3})
	ws.App.Scenarios = ws.App.Scenarios[2:]
	passed := Scenarios(packs)(context.Background(), ws)
	testutil.Equal(t, passed.Status, gate.StatusPass)
	testutil.Equal(t, passed.Values[ValueSecurityScenarios], float64(percentScale))
	ws.App.Scenarios = nil
	testutil.Equal(t, Scenarios(packs)(context.Background(), ws).Status, gate.StatusFail)
	packs[1].Passed = func(context.Context, gate.Workspace) (map[string]bool, error) { return nil, errSim }
	testutil.Equal(t, Scenarios(packs)(context.Background(), ws).Reason, "beta:web: simulated")
	broken := repoWS(t)
	broken.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	testutil.Contains(t, Scenarios(packs)(context.Background(), broken).Reason, "git ls-files")
	blockedOut := repoWS(t, manifestA, "x.go")
	blockedOut.Out = filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blockedOut.Out, "")
	testutil.Equal(t, Scenarios(packs)(context.Background(), blockedOut).Status, gate.StatusFail)
}

func TestCheckNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Checks {
		testutil.True(t, !seen[c], "duplicate "+c)
		seen[c] = true
	}
	testutil.Equal(t, len(Checks), 11)
}

func TestScenariosWithoutCodeAreNotApplicable(t *testing.T) {
	packs := testPacks(nil)
	packs[2].NoCode = true
	packs[2].Passed = func(context.Context, gate.Workspace) (map[string]bool, error) { return map[string]bool{}, nil }
	ws := repoWS(t, "site.sh")
	out := Scenarios(packs)(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusNA)
	testutil.Equal(t, out.Reason, reasonNoScenarios)
	testutil.Equal(t, Scenarios(packs)(context.Background(), repoWS(t, readme)).Status, gate.StatusNA)
	ws.App.Scenarios = []policy.Scenario{{ID: "registered", Category: policy.CategoryBusiness, Tests: []string{testGo}}}
	testutil.Equal(t, Scenarios(packs)(context.Background(), ws).Status, gate.StatusFail)
}
