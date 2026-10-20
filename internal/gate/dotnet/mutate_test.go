package dotnet

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const mutationCoverage = `{"src/Core/Pricing.cs":{"lines":{"5":3,"9":1},"branches":0,"covered_branches":0}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := solution(t)
	testutil.Write(t, w.ws.Report(CoverageReport), mutationCoverage)
	testutil.Write(t, w.ws.Report(TRXReport), trxGreen)
	w.broken = func(src string) bool { return strings.Contains(src, "value >= 3") }
	w.kill = func(src string) error {
		if strings.Contains(src, "value <= 3") || strings.Contains(src, "value - 1") {
			return exitError(t)
		}
		return nil
	}
	return w
}

func TestDotnetMutation(t *testing.T) {
	w := mutationWorld(t)
	rep, err := runMutation(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[mutation.Status]int{mutation.StatusKilled: 2, mutation.StatusSurvived: 1, mutation.StatusNotViable: 1})
	testutil.Equal(t, rep.Survivors, []string{"src/Core/Pricing.cs:9 return-value: return value + 1 -> return default (survived)"})
	for i := range mutation.Workers() {
		testutil.Missing(t, w.ws.Report(sandboxPrefix+strconv.Itoa(i)))
	}
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.ws.Src, filepath.FromSlash(fileCore))), coreSource)
	test, _ := w.fake.Find("test --no-build")
	testutil.Equal(t, test.Args, []string{cmdTest, "--no-build", flagNoRestore, flagVerbosity, verbosityQuiet})
	testutil.True(t, strings.HasPrefix(test.Dir, w.ws.Report(sandboxPrefix)), "mutants run in a sandbox")
	build, _ := w.fake.Find("build -p:RunAnalyzers=false")
	testutil.Equal(t, build.Args, append([]string{"build"}, plainBuild...))
	testutil.Equal(t, Mutation(context.Background(), w.ws).Values[mutation.ValueScore], float64(2)*100/3)
}

func TestDotnetMutationNeedsReports(t *testing.T) {
	noCoverage := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noCoverage.ws.Report(CoverageReport)))
	_, err := runMutation(context.Background(), noCoverage.ws)
	testutil.Contains(t, err.Error(), "coverage report missing")
	noTRX := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noTRX.ws.Report(TRXReport)))
	_, err = runMutation(context.Background(), noTRX.ws)
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
	invalid := mutationWorld(t)
	testutil.Write(t, filepath.Join(invalid.ws.Src, filepath.FromSlash(fileCore)), "var a = \"\xff\";\n")
	_, err = runMutation(context.Background(), invalid.ws)
	testutil.Contains(t, err.Error(), fileCore)
	vanished := mutationWorld(t)
	testutil.NoErr(t, os.Remove(filepath.Join(vanished.ws.Src, filepath.FromSlash(fileCore))))
	_, err = runMutation(context.Background(), vanished.ws)
	testutil.Fails(t, err)
	noSandbox := mutationWorld(t)
	noSandbox.on("build -p:RunAnalyzers=false", "", errSim)
	_, err = runMutation(context.Background(), noSandbox.ws)
	testutil.Is(t, err, mutation.ErrIncomplete)
	ctx := context.Background()
	testutil.Equal(t, classify(ctx, nil), mutation.StatusSurvived)
	testutil.Equal(t, classify(ctx, exitError(t)), mutation.StatusKilled)
	testutil.Equal(t, classify(ctx, errSim), mutation.StatusErrored)
	expired, cancel := context.WithCancel(ctx)
	cancel()
	testutil.Equal(t, classify(expired, exitError(t)), mutation.StatusTimedOut)
	testutil.Equal(t, Mutation(ctx, gate.Workspace{Src: t.TempDir(), Out: t.TempDir()}).Status, gate.StatusFail)
}
