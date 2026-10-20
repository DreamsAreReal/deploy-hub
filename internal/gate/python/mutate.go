package python

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
)

const (
	sandboxPrefix = "mutation-src-"
	venvPrefix    = "mutation-venv-"
	exitFailed    = 1
	exitBroken    = 2
	filePerm      = 0o600
	deadlineScale = 2
)

type worker struct {
	once    sync.Once
	err     error
	sandbox string
	env     env
}

type mutator struct {
	ws       gate.Workspace
	plugins  string
	coverage coverageJSON
	limit    time.Duration
	workers  []*worker
}

func Mutation(ctx context.Context, ws gate.Workspace) gate.Outcome {
	rep, err := runMutation(ctx, ws)
	return mutation.Judge(ws, rep, err)
}

func runMutation(ctx context.Context, ws gate.Workspace) (mutation.Report, error) {
	cov, err := loadCoverage(ws)
	if err != nil {
		return mutation.Report{}, err
	}
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return mutation.Report{}, err
	}
	plugins, err := pluginPath(ws)
	if err != nil {
		return mutation.Report{}, err
	}
	m := &mutator{ws: ws, plugins: plugins, coverage: cov, workers: make([]*worker, mutation.Workers())}
	m.limit = max(ws.Policy.Limits.MutantFloor, time.Duration(junit.Seconds(cases)*float64(time.Second))*time.Duration(ws.Policy.Limits.MutantFactor))
	for i := range m.workers {
		m.workers[i] = &worker{sandbox: ws.Report(sandboxPrefix + strconv.Itoa(i))}
	}
	jobs, err := m.jobs(ctx)
	if err != nil {
		return mutation.Report{}, err
	}
	defer m.cleanup()
	return mutation.Execute(ctx, ws, jobs, len(m.workers))
}

func (m *mutator) jobs(ctx context.Context) ([]mutation.Job, error) {
	files, err := sourceFiles(ctx, m.ws)
	if err != nil {
		return nil, err
	}
	var jobs []mutation.Job
	for _, rel := range files {
		if isTest(rel) || filepath.Ext(rel) != Source {
			continue
		}
		found, ferr := m.fileJobs(rel)
		if ferr != nil {
			return nil, ferr
		}
		jobs = append(jobs, found...)
	}
	return jobs, nil
}

func (m *mutator) fileJobs(rel string) ([]mutation.Job, error) {
	abs := filepath.Join(m.ws.Src, filepath.FromSlash(rel))
	data, err := os.ReadFile(filepath.Clean(abs))
	if err != nil {
		return nil, err
	}
	points, err := tokmut.Points(abs, data, tokmut.Python)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	contexts := m.coverage.Files[rel].Contexts
	jobs := make([]mutation.Job, 0, len(points))
	for _, p := range points {
		lineContexts, covered := contexts[strconv.Itoa(p.Line)]
		tests := testsOf(lineContexts)
		jobs = append(jobs, mutation.Job{Mutant: p, Run: func(ctx context.Context, w int) mutation.Status {
			if !covered {
				return mutation.StatusNotCovered
			}
			return m.run(ctx, w, rel, data, p, tests)
		}})
	}
	return jobs, nil
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

func (m *mutator) ready(ctx context.Context, w int) (*worker, error) {
	wk := m.workers[w]
	wk.once.Do(func() {
		ws := m.ws
		wk.err = source.CopyTracked(ctx, ws, wk.sandbox)
		wk.env = newEnv(ws, wk.sandbox, ws.Report(venvPrefix+strconv.Itoa(w)))
		if wk.err == nil {
			wk.err = wk.env.create(ctx)
		}
		if wk.err == nil {
			wk.err = wk.env.withTestTools(ctx)
		}
	})
	return wk, wk.err
}

func (m *mutator) run(ctx context.Context, w int, rel string, original []byte, p mutation.Mutant, tests []string) mutation.Status {
	wk, err := m.ready(ctx, w)
	if err != nil {
		m.ws.Log.Error("mutation sandbox not ready", "error", err.Error())
		return mutation.StatusErrored
	}
	root, err := os.OpenRoot(wk.sandbox)
	if err != nil {
		m.ws.Log.Error("mutant staging failed", "file", rel, "error", err.Error())
		return mutation.StatusErrored
	}
	defer func() { _ = root.Close() }()
	defer func() { _ = root.WriteFile(rel, original, filePerm) }()
	if werr := root.WriteFile(rel, p.Apply(original), filePerm); werr != nil {
		m.ws.Log.Error("mutant staging failed", "file", rel, "error", werr.Error())
		return mutation.StatusErrored
	}
	if _, cerr := wk.env.python(ctx, nil, flagModule, "py_compile", filepath.Join(wk.sandbox, filepath.FromSlash(rel))); cerr != nil {
		return mutation.StatusNotViable
	}
	tctx, cancel := context.WithTimeout(ctx, deadlineScale*m.limit+m.ws.Policy.Limits.MutantFloor)
	defer cancel()
	envs := testEnv(m.ws, m.plugins, m.ws.Report(hypothesisDir+"-"+strconv.Itoa(w)))
	_, runErr := wk.env.python(tctx, envs, pytestArgs(append([]string{"-x"}, tests...)...)...)
	return classify(tctx, runErr)
}

func classify(ctx context.Context, err error) mutation.Status {
	code := exitCode(err)
	switch {
	case err == nil:
		return mutation.StatusSurvived
	case ctx.Err() != nil:
		return mutation.StatusTimedOut
	case code == exitFailed || code == exitBroken:
		return mutation.StatusKilled
	default:
		return mutation.StatusErrored
	}
}

func (m *mutator) cleanup() {
	for i, wk := range m.workers {
		_ = os.RemoveAll(wk.sandbox)
		_ = os.RemoveAll(m.ws.Report(venvPrefix + strconv.Itoa(i)))
	}
}
