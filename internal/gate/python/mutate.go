package python

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	venvPrefix = "mutation-venv-"
	exitFailed = 1
	exitBroken = 2
)

var Mutation = tokmut.Check(mutationSuite)

func mutationSuite(ctx context.Context, ws gate.Workspace) (tokmut.Suite, error) {
	cov, err := loadCoverage(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return tokmut.Suite{}, err
	}
	plugins, err := pluginPath(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	files, err := sourceFiles(ctx, ws)
	files = slices.DeleteFunc(files, func(f string) bool { return isTest(f) || filepath.Ext(f) != Source })
	envs := make([]env, mutation.Workers())
	venv := func(w int) string { return ws.Report(venvPrefix + strconv.Itoa(w)) }
	return tokmut.Suite{
		Family:  tokmut.Python,
		Files:   files,
		Seconds: junit.Seconds(cases),
		Covered: func(rel string, line int) (bool, []string) {
			contexts, covered := cov.Files[rel].Contexts[strconv.Itoa(line)]
			return covered, testsOf(contexts)
		},
		Prepare: func(ctx context.Context, w int, dir string) error {
			envs[w] = newEnv(ws, dir, venv(w))
			if perr := envs[w].create(ctx); perr != nil {
				return perr
			}
			return envs[w].withTestTools(ctx)
		},
		Compile: func(ctx context.Context, w int, dir, rel string) error {
			_, cerr := envs[w].python(ctx, nil, flagModule, "py_compile", filepath.Join(dir, filepath.FromSlash(rel)))
			return cerr
		},
		Test: func(ctx context.Context, w int, _ string, tests []string) error {
			hypothesis := testEnv(ws, plugins, ws.Report(hypothesisDir+"-"+strconv.Itoa(w)))
			_, terr := envs[w].python(ctx, hypothesis, pytestArgs(append([]string{"-x"}, tests...)...)...)
			return terr
		},
		Killed: func(err error) bool {
			code := procexec.ExitCode(err)
			return code == exitFailed || code == exitBroken
		},
		Cleanup: func(w int) { _ = os.RemoveAll(venv(w)) },
	}, err
}

func testsOf(contexts []string) []string {
	if slices.Contains(contexts, "") {
		return nil
	}
	var out []string
	for _, c := range contexts {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}
