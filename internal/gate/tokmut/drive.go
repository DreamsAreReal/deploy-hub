package tokmut

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	SandboxPrefix = "mutation-src-"
	sandboxPerm   = 0o600
	deadlineScale = 2
)

type Suite struct {
	Family  Family
	Files   []string
	Seconds float64
	Covered func(rel string, line int) (bool, []string)
	Prepare func(ctx context.Context, worker int, sandbox string) error
	Compile func(ctx context.Context, worker int, sandbox, rel string) error
	Test    func(ctx context.Context, worker int, sandbox string, tests []string) error
	Killed  func(err error) bool
	Cleanup func(worker int)
}

type sandbox struct {
	once sync.Once
	err  error
	dir  string
}

type driver struct {
	ws    gate.Workspace
	suite Suite
	limit time.Duration
	boxes []*sandbox
}

type Build func(context.Context, gate.Workspace) (Suite, error)

func Check(build Build) func(context.Context, gate.Workspace) gate.Outcome {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		rep, err := Mutate(ctx, ws, build)
		return mutation.Judge(ws, rep, err)
	}
}

func Mutate(ctx context.Context, ws gate.Workspace, build Build) (mutation.Report, error) {
	ws.MemoryLimit = mutation.MemoryBudget()
	s, err := build(ctx, ws)
	if err != nil {
		return mutation.Report{}, err
	}
	return Run(ctx, ws, s)
}

func Run(ctx context.Context, ws gate.Workspace, s Suite) (mutation.Report, error) {
	d := &driver{ws: ws, suite: s, boxes: make([]*sandbox, mutation.Workers())}
	d.limit = max(ws.Policy.Limits.MutantFloor, time.Duration(s.Seconds*float64(time.Second))*time.Duration(ws.Policy.Limits.MutantFactor))
	for i := range d.boxes {
		d.boxes[i] = &sandbox{dir: ws.Report(SandboxPrefix + strconv.Itoa(i))}
	}
	var jobs []mutation.Job
	for _, rel := range s.Files {
		found, err := d.fileJobs(rel)
		if err != nil {
			return mutation.Report{}, err
		}
		jobs = append(jobs, found...)
	}
	defer d.cleanup()
	return mutation.Execute(ctx, ws, jobs, len(d.boxes))
}

func (d *driver) fileJobs(rel string) ([]mutation.Job, error) {
	abs := filepath.Join(d.ws.Src, filepath.FromSlash(rel))
	data, err := os.ReadFile(filepath.Clean(abs))
	if err != nil {
		return nil, err
	}
	points, err := Points(abs, data, d.suite.Family)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	jobs := make([]mutation.Job, 0, len(points))
	for _, p := range points {
		covered, tests := d.suite.Covered(rel, p.Line)
		jobs = append(jobs, mutation.Job{Mutant: p, Run: func(ctx context.Context, w int) mutation.Status {
			if !covered {
				return mutation.StatusNotCovered
			}
			return d.run(ctx, w, rel, data, p, tests)
		}})
	}
	return jobs, nil
}

func (d *driver) ready(ctx context.Context, w int) (string, error) {
	box := d.boxes[w]
	box.once.Do(func() {
		box.err = source.CopyTracked(ctx, d.ws, box.dir)
		if box.err == nil && d.suite.Prepare != nil {
			box.err = d.suite.Prepare(ctx, w, box.dir)
		}
	})
	return box.dir, box.err
}

func (d *driver) run(ctx context.Context, w int, rel string, original []byte, p mutation.Mutant, tests []string) mutation.Status {
	dir, err := d.ready(ctx, w)
	if err != nil {
		d.ws.Log.Error("mutation sandbox not ready", "error", err.Error())
		return mutation.StatusErrored
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		d.ws.Log.Error("mutant staging failed", "file", rel, "error", err.Error())
		return mutation.StatusErrored
	}
	defer func() { _ = root.Close() }()
	defer func() { _ = root.WriteFile(rel, original, sandboxPerm) }()
	if werr := root.WriteFile(rel, p.Apply(original), sandboxPerm); werr != nil {
		d.ws.Log.Error("mutant staging failed", "file", rel, "error", werr.Error())
		return mutation.StatusErrored
	}
	if cerr := d.suite.Compile(ctx, w, dir, rel); cerr != nil {
		return mutation.StatusNotViable
	}
	tctx, cancel := context.WithTimeout(ctx, deadlineScale*d.limit+d.ws.Policy.Limits.MutantFloor)
	defer cancel()
	return d.classify(tctx, d.suite.Test(tctx, w, dir, tests))
}

func (d *driver) classify(ctx context.Context, err error) mutation.Status {
	switch {
	case err == nil:
		return mutation.StatusSurvived
	case errors.Is(err, procexec.ErrMemoryLimit):
		return mutation.StatusExhausted
	case ctx.Err() != nil:
		return mutation.StatusTimedOut
	case d.killed(err):
		return mutation.StatusKilled
	default:
		return mutation.StatusErrored
	}
}

func (d *driver) killed(err error) bool {
	if d.suite.Killed != nil {
		return d.suite.Killed(err)
	}
	return procexec.ExitCode(err) > 0
}

func (d *driver) cleanup() {
	for i, box := range d.boxes {
		_ = os.RemoveAll(box.dir)
		if d.suite.Cleanup != nil {
			d.suite.Cleanup(i)
		}
	}
}
