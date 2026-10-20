package dotnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
)

const (
	sandboxPrefix = "mutation-src-"
	filePerm      = 0o600
	deadlineScale = 2
)

var plainBuild = append(slices.Clone(quietAnalyzers), "-p:GenerateDocumentationFile=false", flagVerbosity, verbosityQuiet)

type worker struct {
	once    sync.Once
	err     error
	sandbox string
}

type mutator struct {
	ws      gate.Workspace
	cov     map[string]*fileCoverage
	limit   time.Duration
	workers []*worker
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
	cases, err := loadTRX(ws.Report(TRXReport))
	if err != nil {
		return mutation.Report{}, err
	}
	m := &mutator{ws: ws, cov: cov, workers: make([]*worker, mutation.Workers())}
	total := 0.0
	for _, c := range cases {
		total += c.Time
	}
	m.limit = max(ws.Policy.Limits.MutantFloor, time.Duration(total*float64(time.Second))*time.Duration(ws.Policy.Limits.MutantFactor))
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
	files, err := trackedFiles(ctx, m.ws, func(f string) bool { return path.Ext(f) == csharpSource && !isTest(f) })
	if err != nil {
		return nil, err
	}
	var jobs []mutation.Job
	for _, rel := range files {
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
	points, err := tokmut.Points(abs, data, tokmut.CSharp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	lines := map[int]int{}
	if f := m.cov[rel]; f != nil {
		lines = f.Lines
	}
	jobs := make([]mutation.Job, 0, len(points))
	for _, p := range points {
		hits := lines[p.Line]
		jobs = append(jobs, mutation.Job{Mutant: p, Run: func(ctx context.Context, w int) mutation.Status {
			if hits == 0 {
				return mutation.StatusNotCovered
			}
			return m.run(ctx, w, rel, data, p)
		}})
	}
	return jobs, nil
}

func (m *mutator) ready(ctx context.Context, w int) (*worker, error) {
	wk := m.workers[w]
	wk.once.Do(func() {
		wk.err = source.CopyTracked(ctx, m.ws, wk.sandbox)
		if wk.err == nil {
			_, wk.err = run(ctx, m.ws, wk.sandbox, nil, append([]string{cmdBuild}, plainBuild...)...)
		}
	})
	return wk, wk.err
}

func (m *mutator) run(ctx context.Context, w int, rel string, original []byte, p mutation.Mutant) mutation.Status {
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
	if _, berr := run(ctx, m.ws, wk.sandbox, nil, append([]string{cmdBuild, flagNoRestore}, plainBuild...)...); berr != nil {
		return mutation.StatusNotViable
	}
	tctx, cancel := context.WithTimeout(ctx, deadlineScale*m.limit+m.ws.Policy.Limits.MutantFloor)
	defer cancel()
	_, runErr := run(tctx, m.ws, wk.sandbox, nil, cmdTest, "--no-build", flagNoRestore, flagVerbosity, verbosityQuiet)
	return classify(tctx, runErr)
}

func classify(ctx context.Context, err error) mutation.Status {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return mutation.StatusSurvived
	case ctx.Err() != nil:
		return mutation.StatusTimedOut
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return mutation.StatusKilled
	default:
		return mutation.StatusErrored
	}
}

func (m *mutator) cleanup() {
	for _, wk := range m.workers {
		_ = os.RemoveAll(wk.sandbox)
	}
}
