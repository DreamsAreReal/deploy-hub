package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const mutationCoverage = `{"src/cart.ts":{"lines":{"1":1,"4":2,"8":0},"branches":0,"covered_branches":0}}`

func mutationWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, fileSpec: "x", viteConfig: "", readme: ""})
	staged(t, w)
	testutil.Write(t, w.WS.Report(linecov.ReportFile), mutationCoverage)
	testutil.Write(t, w.WS.Report(JUnitReport), junitGreen)
	w.Broken = func(src string) bool { return strings.Contains(src, "value >= LIMIT") }
	w.Kill = func(src string) error {
		switch {
		case strings.Contains(src, "value <= LIMIT"):
			return packtest.ExitError(t, 1)
		case strings.Contains(src, "LIMIT = 4"):
			return packtest.ExitError(t, 1)
		default:
			return nil
		}
	}
	return w
}

func TestNodeMutation(t *testing.T) {
	w := mutationWorld(t)
	rep, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Survivors, []string{
		"src/cart.ts:4 return-value: return value > LIMIT -> return null (survived)",
		"src/cart.ts:8 return-value: return value + 1 -> return null (not-covered)",
		"src/cart.ts:8 arithmetic: + -> - (not-covered)",
	})
	bail, _ := w.Fake.Find("--bail=1")
	testutil.Equal(t, bail.Args, []string{local(w.WS, "vitest", "vitest.mjs"), cmdRun, "--bail=1", "--reporter=dot"})
	testutil.True(t, strings.HasPrefix(bail.Dir, w.WS.Report(tokmut.SandboxPrefix)), "mutants run in a sandbox")
	syntax, _ := w.Fake.Find(cmdSyntax)
	testutil.Equal(t, syntax.Args[0], filepath.Join(tools(w.WS), syntaxCheckFile))
	out := Mutation(context.Background(), w.WS)
	testutil.Equal(t, out.Values[mutation.ValueScore], float64(25))
}

func TestNodeMutationNeedsReports(t *testing.T) {
	packtest.Breaks(t, mutationWorld, runMutation, map[string]func(*world){
		Manifest:                  func(w *world) { testutil.NoErr(t, os.Remove(filepath.Join(stage(w.WS), Manifest))) },
		"coverage report missing": func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(linecov.ReportFile))) },
		JUnitReport:               func(w *world) { testutil.NoErr(t, os.Remove(w.WS.Report(JUnitReport))) },
		"git ls-files":            func(w *world) { w.FailGit() },
	})
}

func runMutation(w *world) error {
	_, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	return err
}

func TestNodeMutationFailsWithoutAProject(t *testing.T) {
	testutil.Equal(t, Mutation(context.Background(), gate.Workspace{Src: t.TempDir(), Out: t.TempDir()}).Status, gate.StatusFail)
}
