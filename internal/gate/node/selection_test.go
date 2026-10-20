package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	plainSource = "export const twice = (value) => value * 2;\n\nexport function above(value) {\n  return value > 3;\n}\n"
	plainFile   = "src/limit.js"
	coversTest  = "test/limit.test.js"
	importsTest = "src/other.test.mjs"
	plainLcov   = `{"src/limit.js":{"lines":{"1":1,"4":1},"branches":0,"covered_branches":0}}`
)

func TestNodeTestFilesFollowTheRunnerDefaults(t *testing.T) {
	for _, rel := range []string{"a.test.js", "src/b-test.mjs", "c_test.cjs", "test-d.js", "test.js", "test/e.js", "pkg/test/deep/f.mjs"} {
		testutil.True(t, nodeTestFile(rel), rel)
	}
	for _, rel := range []string{"a.js", "src/testing.js", "latest.js", "a.test.ts", "test/data.json", "pkg/testdata/x.test.js", "attest.js", "contest-a.js"} {
		testutil.True(t, !nodeTestFile(rel), rel)
	}
}

func selectionWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood, plainFile: plainSource, coversTest: "t", importsTest: "o", "README.md": ""})
	staged(t, w)
	testutil.Write(t, w.WS.Report(linecov.ReportFile), plainLcov)
	testutil.Write(t, w.WS.Report(JUnitReport), junitGreen)
	hook := w.Hook
	w.Hook = func(c procexec.Command, line string) (procexec.Result, error) {
		if report, ok := strings.CutPrefix(c.Args[max(0, len(c.Args)-2)], "--test-reporter-destination="); ok && len(c.Args) > 1 {
			lines := "DA:1,1\nDA:4,1\n"
			if c.Args[len(c.Args)-1] == importsTest {
				lines = "DA:1,1\nDA:4,0\n"
			}
			if info, serr := os.Stat(report); serr != nil || !info.IsDir() {
				testutil.Write(t, report, "SF:"+plainFile+"\n"+lines+"end_of_record\n")
			}
			return procexec.Result{}, packtest.ErrSim
		}
		return hook(c, line)
	}
	return w
}

func TestTheIndexMapsLinesToTheTestFilesThatRunThem(t *testing.T) {
	w := selectionWorld(t)
	index, err := buildIndex(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, index.tests(plainFile, 1), []string{importsTest, coversTest})
	testutil.Equal(t, index.tests(plainFile, 4), []string{coversTest})
	testutil.Equal(t, len(index.tests(plainFile, 2)), 0)
	testutil.Equal(t, len(index.tests("src/none.js", 1)), 0)
	call, _ := w.Fake.Find(coversTest)
	testutil.Equal(t, call.Args[:3], []string{flagTest, "--experimental-test-coverage", "--test-reporter=lcov"})
	testutil.Equal(t, call.Dir, stage(w.WS))
}

func TestNodeMutantsRunOnlyTheCoveringTestFiles(t *testing.T) {
	w := selectionWorld(t)
	var ran [][]string
	hook := w.Hook
	w.Hook = func(c procexec.Command, line string) (procexec.Result, error) {
		if c.Args[0] == flagTest && len(c.Args) > 1 && !strings.HasPrefix(c.Args[1], "--") {
			ran = append(ran, slices.Clone(c.Args[1:]))
			return procexec.Result{}, packtest.ExitError(t, 1)
		}
		if len(c.Args) == 1 && c.Args[0] == flagTest {
			ran = append(ran, nil)
			return procexec.Result{}, packtest.ExitError(t, 1)
		}
		return hook(c, line)
	}
	rep, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(rep.Survivors), 0)
	testutil.True(t, slices.ContainsFunc(ran, func(args []string) bool { return slices.Equal(args, []string{coversTest}) }), "line 4 runs only its test")
	testutil.True(t, slices.ContainsFunc(ran, func(args []string) bool { return slices.Equal(args, []string{importsTest, coversTest}) }), "line 1 runs both")
}

func TestNodeMutantsWithoutAnIndexedTestRunTheWholeSuite(t *testing.T) {
	w := selectionWorld(t)
	testutil.Write(t, w.WS.Report(linecov.ReportFile), `{"src/limit.js":{"lines":{"1":1,"4":1,"5":1},"branches":0,"covered_branches":0}}`)
	suite, err := mutationSuite(context.Background(), w.WS)
	testutil.NoErr(t, err)
	hit, tests := suite.Covered(plainFile, 5)
	testutil.True(t, hit && len(tests) == 0, "covered without an index entry")
	testutil.NoErr(t, suite.Test(context.Background(), 0, stage(w.WS), nil))
	last := w.Fake.Calls()[len(w.Fake.Calls())-1]
	testutil.Equal(t, last.Args, []string{flagTest})
}

func TestTheIndexFailsLoudly(t *testing.T) {
	broken := selectionWorld(t)
	broken.FailGit()
	_, err := buildIndex(context.Background(), broken.WS)
	testutil.Fails(t, err)
	_, err = mutationSuite(context.Background(), broken.WS)
	testutil.Fails(t, err)
	blocked := selectionWorld(t)
	testutil.Write(t, blocked.WS.Report(indexDir), "a file where the folder goes")
	_, err = buildIndex(context.Background(), blocked.WS)
	testutil.Fails(t, err)
	unreadable := selectionWorld(t)
	testutil.Mkdir(t, filepath.Join(unreadable.WS.Report(indexDir), "0"+indexExt))
	_, err = buildIndex(context.Background(), unreadable.WS)
	testutil.Fails(t, err)
	testutil.NoErr(t, os.RemoveAll(unreadable.WS.Report(indexDir)))
}
