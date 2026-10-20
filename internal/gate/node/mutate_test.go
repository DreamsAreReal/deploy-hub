package node

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

const mutationCoverage = `{"src/cart.ts":{"lines":{"1":1,"4":2,"8":0},"branches":0,"covered_branches":0}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, fileSpec: "x", viteConfig: "", readme: ""})
	staged(t, w)
	testutil.Write(t, w.ws.Report(CoverageReport), mutationCoverage)
	testutil.Write(t, w.ws.Report(JUnitReport), junitGreen)
	w.broken = func(src string) bool { return strings.Contains(src, "value >= LIMIT") }
	w.kill = func(src string) error {
		switch {
		case strings.Contains(src, "value <= LIMIT"):
			return exitError(t)
		case strings.Contains(src, "LIMIT = 4"):
			return exitError(t)
		default:
			return nil
		}
	}
	return w
}

func TestNodeMutation(t *testing.T) {
	w := mutationWorld(t)
	rep, err := runMutation(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[mutation.Status]int{
		mutation.StatusKilled: 1, mutation.StatusSurvived: 1, mutation.StatusNotViable: 1, mutation.StatusNotCovered: 2,
	})
	testutil.Equal(t, rep.Survivors, []string{
		"src/cart.ts:4 return-value: return value > LIMIT -> return null (survived)",
		"src/cart.ts:8 return-value: return value + 1 -> return null (not-covered)",
		"src/cart.ts:8 arithmetic: + -> - (not-covered)",
	})
	for i := range mutation.Workers() {
		testutil.Missing(t, w.ws.Report(sandboxPrefix+strconv.Itoa(i)))
	}
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.ws.Src, filepath.FromSlash(fileCart))), cartSource)
	bail, _ := w.fake.Find("--bail=1")
	testutil.Equal(t, bail.Args, []string{local(w.ws, "vitest", "vitest.mjs"), cmdRun, "--bail=1", "--reporter=dot"})
	testutil.True(t, strings.HasPrefix(bail.Dir, w.ws.Report(sandboxPrefix)), "mutants run in a sandbox")
	syntax, _ := w.fake.Find(cmdSyntax)
	testutil.Equal(t, syntax.Args[0], filepath.Join(tools(w.ws), syntaxCheckFile))
	out := Mutation(context.Background(), w.ws)
	testutil.Equal(t, out.Values[mutation.ValueScore], float64(25))
}

func TestNodeMutationNeedsReports(t *testing.T) {
	noManifest := mutationWorld(t)
	testutil.NoErr(t, os.Remove(filepath.Join(stage(noManifest.ws), Manifest)))
	_, err := runMutation(context.Background(), noManifest.ws)
	testutil.Fails(t, err)
	noCoverage := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noCoverage.ws.Report(CoverageReport)))
	_, err = runMutation(context.Background(), noCoverage.ws)
	testutil.Contains(t, err.Error(), "coverage report missing")
	noResults := mutationWorld(t)
	testutil.NoErr(t, os.Remove(noResults.ws.Report(JUnitReport)))
	_, err = runMutation(context.Background(), noResults.ws)
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
	testutil.Write(t, filepath.Join(invalid.ws.Src, filepath.FromSlash(fileCart)), "const a = \"\xff\";\n")
	_, err = runMutation(context.Background(), invalid.ws)
	testutil.Contains(t, err.Error(), fileCart)
	vanished := mutationWorld(t)
	testutil.NoErr(t, os.Remove(filepath.Join(vanished.ws.Src, filepath.FromSlash(fileCart))))
	_, err = runMutation(context.Background(), vanished.ws)
	testutil.Fails(t, err)
	noSandbox := mutationWorld(t)
	for i := range mutation.Workers() {
		testutil.Write(t, noSandbox.ws.Report(sandboxPrefix+strconv.Itoa(i)), "")
	}
	_, err = runMutation(context.Background(), noSandbox.ws)
	testutil.Is(t, err, mutation.ErrIncomplete)
}

func TestNodeMutantOutcomes(t *testing.T) {
	ctx := context.Background()
	testutil.Equal(t, classify(ctx, nil), mutation.StatusSurvived)
	testutil.Equal(t, classify(ctx, exitError(t)), mutation.StatusKilled)
	testutil.Equal(t, classify(ctx, errSim), mutation.StatusErrored)
	expired, cancel := context.WithCancel(ctx)
	cancel()
	testutil.Equal(t, classify(expired, exitError(t)), mutation.StatusTimedOut)
	testutil.Equal(t, Mutation(ctx, gate.Workspace{Src: t.TempDir(), Out: t.TempDir()}).Status, gate.StatusFail)
}
