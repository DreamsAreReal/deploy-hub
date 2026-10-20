package dotnet

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const mutationCoverage = `{"src/Core/Pricing.cs":{"lines":{"5":3,"9":1},"branches":0,"covered_branches":0}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := solution(t)
	testutil.Write(t, w.WS.Report(linecov.ReportFile), mutationCoverage)
	testutil.Write(t, w.WS.Report(TRXReport), trxGreen)
	w.Broken = func(src string) bool { return strings.Contains(src, "value >= 3") }
	w.Kill = func(src string) error {
		if strings.Contains(src, "value <= 3") || strings.Contains(src, "value - 1") {
			return packtest.ExitError(t, 1)
		}
		return nil
	}
	return w
}

func TestDotnetMutation(t *testing.T) {
	w := mutationWorld(t)
	rep, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Survivors, []string{"src/Core/Pricing.cs:9 return-value: return value + 1 -> return default (survived)"})
	test, _ := w.Fake.Find("test --no-build")
	testutil.Equal(t, test.Args, []string{cmdTest, "--no-build", flagNoRestore, flagVerbosity, verbosityQuiet})
	testutil.True(t, strings.HasPrefix(test.Dir, w.WS.Report(tokmut.SandboxPrefix)), "mutants run in a sandbox")
	build, _ := w.Fake.Find("build -p:RunAnalyzers=false")
	testutil.Equal(t, build.Args, append([]string{"build"}, plainBuild...))
	testutil.Equal(t, Mutation(context.Background(), w.WS).Values[mutation.ValueScore], float64(2)*100/3)
}

func TestDotnetMutationNeedsReports(t *testing.T) {
	packtest.Breaks(t, mutationWorld, runMutation, map[string]func(*world){
		"coverage report missing":      func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(linecov.ReportFile))) },
		TRXReport:                      func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(TRXReport))) },
		"git ls-files":                 func(w *world) { w.FailGit() },
		mutation.ErrIncomplete.Error(): func(w *world) { w.On("build -p:RunAnalyzers=false", "", errSim) },
	})
	testutil.Equal(t, Mutation(context.Background(), gate.Workspace{Src: t.TempDir(), Out: t.TempDir()}).Status, gate.StatusFail)
}

func runMutation(w *world) error {
	_, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	return err
}
