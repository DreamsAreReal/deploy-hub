package python

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const mutationCoverage = `{"totals":{},"files":{"src/shop/app.py":{"contexts":{"1":[""],"4":[""],"5":["` + testOK + `"]}}}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{lockFile: "", ProjectFile: "", fileApp: appSource, fileTest: "def test_ok() -> None:\n    assert True\n", readmeFile: "x"})
	testutil.Write(t, w.ws.Report(CoverageReport), mutationCoverage)
	testutil.Write(t, w.ws.Report(JUnitReport), junitGreen)
	w.broken = func(src string) bool { return strings.Contains(src, "value >= LIMIT") }
	w.kill = func(src string) error {
		switch {
		case strings.HasPrefix(src, "pass"):
			return exitError(t, "2")
		case strings.Contains(src, "value <= LIMIT"):
			return exitError(t, "1")
		default:
			return nil
		}
	}
	return w
}

func TestMutationClassifiesEveryMutant(t *testing.T) {
	w := mutationWorld(t)
	rep, err := runMutation(context.Background(), w.ws)
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
	for _, line := range w.fake.Lines() {
		if strings.Contains(line, cmdPytest) && strings.HasSuffix(line, "-x "+testOK) {
			selective++
		}
	}
	testutil.Equal(t, selective, 2)
	testutil.True(t, w.fake.Count(cmdPytest+" -p deploy_hub_pytest -p no:cacheprovider -o addopts= -q -x") == 3, "three mutants ran tests")
	for i := range mutation.Workers() {
		testutil.Missing(t, w.ws.Report(sandboxPrefix+itoa(i)))
		testutil.Missing(t, w.ws.Report(venvPrefix+itoa(i)))
	}
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.ws.Src, filepath.FromSlash(fileApp))), appSource)
	sync, _ := w.fake.Find(cmdSync)
	testutil.Contains(t, sync.Env[2], envProject+w.ws.Report(venvPrefix))
}

func TestMutationCheckJudgesTheScore(t *testing.T) {
	w := mutationWorld(t)
	out := Mutation(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[mutation.ValueScore], float64(40))
	_, err := os.Stat(w.ws.Report(mutation.ReportFile))
	testutil.NoErr(t, err)
}

func TestMutationNeedsReportsAndSandboxes(t *testing.T) {
	noCoverage := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noCoverage.ws.Report(CoverageReport)))
	_, err := runMutation(context.Background(), noCoverage.ws)
	testutil.Contains(t, err.Error(), "coverage report missing")
	noJUnit := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noJUnit.ws.Report(JUnitReport)))
	_, err = runMutation(context.Background(), noJUnit.ws)
	testutil.Fails(t, err)
	blocked := mutationWorld(t)
	testutil.Write(t, blocked.ws.Report(pluginDir), "")
	_, err = runMutation(context.Background(), blocked.ws)
	testutil.Fails(t, err)
	unlisted := mutationWorld(t)
	unlisted.fake.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Name == gitBinary {
			return procexec.Result{}, errSim
		}
		return unlisted.handle(c)
	}
	_, err = runMutation(context.Background(), unlisted.ws)
	testutil.Contains(t, err.Error(), "git ls-files")
	unreadable := mutationWorld(t)
	testutil.Write(t, filepath.Join(unreadable.ws.Src, filepath.FromSlash(fileApp)), "x = \"\xff\"\n")
	_, err = runMutation(context.Background(), unreadable.ws)
	testutil.Contains(t, err.Error(), fileApp)
	vanished := mutationWorld(t)
	testutil.NoErr(t, os.Remove(filepath.Join(vanished.ws.Src, filepath.FromSlash(fileApp))))
	_, err = runMutation(context.Background(), vanished.ws)
	testutil.Fails(t, err)
	noSandbox := mutationWorld(t)
	noSandbox.on(cmdSync, "", errSim)
	_, err = runMutation(context.Background(), noSandbox.ws)
	testutil.Is(t, err, mutation.ErrIncomplete)
}

func TestMutantOutcomes(t *testing.T) {
	ctx := context.Background()
	testutil.Equal(t, classify(ctx, nil), mutation.StatusSurvived)
	testutil.Equal(t, classify(ctx, exitError(t, "1")), mutation.StatusKilled)
	testutil.Equal(t, classify(ctx, exitError(t, "2")), mutation.StatusKilled)
	testutil.Equal(t, classify(ctx, exitError(t, "3")), mutation.StatusErrored)
	testutil.Equal(t, classify(ctx, errSim), mutation.StatusErrored)
	expired, cancel := context.WithCancel(ctx)
	cancel()
	testutil.Equal(t, classify(expired, exitError(t, "1")), mutation.StatusTimedOut)
	testutil.Equal(t, procexectest.Line(procexec.Command{Name: UVBinary, Args: []string{"run"}}), "uv run")
}
