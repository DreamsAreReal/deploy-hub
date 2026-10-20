package python

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const mutationCoverage = `{"totals":{},"files":{"src/shop/app.py":{"contexts":{"1":[""],"4":[""],"5":["` + testOK + `"]}}}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{lockFile: "", ProjectFile: "", fileApp: appSource, fileTest: "def test_ok() -> None:\n    assert True\n", readmeFile: "x"})
	testutil.Write(t, w.WS.Report(CoverageReport), mutationCoverage)
	testutil.Write(t, w.WS.Report(JUnitReport), junitGreen)
	w.Broken = func(src string) bool { return strings.Contains(src, "value >= LIMIT") }
	w.Kill = func(src string) error {
		switch {
		case strings.HasPrefix(src, "pass"):
			return packtest.ExitError(t, 2)
		case strings.Contains(src, "value <= LIMIT"):
			return packtest.ExitError(t, 1)
		default:
			return nil
		}
	}
	return w
}

func TestMutationClassifiesEveryMutant(t *testing.T) {
	w := mutationWorld(t)
	rep, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[mutation.Status]int{
		mutation.StatusKilled: 2, mutation.StatusSurvived: 1, mutation.StatusNotViable: 1, mutation.StatusNotCovered: 2,
	})
	testutil.Equal(t, rep.Survivors, []string{
		"src/shop/app.py:5 return-value: return value > LIMIT -> return None (survived)",
		"src/shop/app.py:9 return-value: return value + 1 -> return None (not-covered)",
		"src/shop/app.py:9 arithmetic: + -> - (not-covered)",
	})
	selective := 0
	for _, line := range w.Fake.Lines() {
		if strings.Contains(line, cmdPytest) && strings.HasSuffix(line, "-x "+testOK) {
			selective++
		}
	}
	testutil.Equal(t, selective, 2)
	testutil.True(t, w.Fake.Count(cmdPytest+" -p deploy_hub_pytest -p no:cacheprovider -o addopts= -q -x") == 3, "three mutants ran tests")
	for i := range mutation.Workers() {
		testutil.Missing(t, w.WS.Report(venvPrefix+itoa(i)))
	}
	sync, _ := w.Fake.Find(cmdSync)
	testutil.Contains(t, sync.Env[2], envProject+w.WS.Report(venvPrefix))
}

func TestMutationCheckJudgesTheScore(t *testing.T) {
	w := mutationWorld(t)
	out := Mutation(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[mutation.ValueScore], float64(40))
	_, err := os.Stat(w.WS.Report(mutation.ReportFile))
	testutil.NoErr(t, err)
}

func TestMutationNeedsReportsAndSandboxes(t *testing.T) {
	packtest.Breaks(t, mutationWorld, runMutation, map[string]func(*world){
		"coverage report missing":      func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(CoverageReport))) },
		JUnitReport:                    func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(JUnitReport))) },
		pluginDir:                      func(w *world) { testutil.Write(t, w.WS.Report(pluginDir), "") },
		"git ls-files":                 func(w *world) { w.FailGit() },
		mutation.ErrIncomplete.Error(): func(w *world) { w.On(cmdSync, "", errSim) },
	})
}

func runMutation(w *world) error {
	_, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	return err
}

func TestMutantOutcomes(t *testing.T) {
	s, err := mutationSuite(context.Background(), mutationWorld(t).WS)
	testutil.NoErr(t, err)
	testutil.True(t, s.Killed(packtest.ExitError(t, 1)) && s.Killed(packtest.ExitError(t, 2)), "failed and interrupted pytest runs kill the mutant")
	testutil.True(t, !s.Killed(packtest.ExitError(t, 3)) && !s.Killed(errSim), "usage and internal errors do not")
	testutil.Equal(t, procexectest.Line(procexec.Command{Name: UVBinary, Args: []string{"run"}}), "uv run")
}
