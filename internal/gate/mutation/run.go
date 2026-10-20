package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

type Status string

const (
	StatusKilled     Status = "killed"
	StatusSurvived   Status = "survived"
	StatusNotCovered Status = "not-covered"
	StatusTimedOut   Status = "timed-out"
	StatusNotViable  Status = "not-viable"
	StatusErrored    Status = "errored"

	ValueScore      = "mutation_score"
	ReportFile      = "mutation.json"
	ValueMutants    = "mutants"
	mutantsDir      = "mutants"
	sandboxDir      = "mutation-src"
	overlayFile     = "overlay.json"
	overlayEnv      = "GOFLAGS=-overlay="
	cacheDir        = "mutation-cache"
	cacheEnv        = "GOCACHE="
	testFailed      = "FAIL"
	buildFailed     = "[build failed]"
	setupFailed     = "[setup failed]"
	testTimedOut    = "panic: test timed out"
	percentScale    = 100
	filePerm        = 0o600
	dirPerm         = 0o700
	snippetLimit    = 60
	cpuShare        = 2
	progressSteps   = 20
	minWorkers      = 2
	reasonScore     = "mutation score is below the policy threshold"
	reasonIncomplet = "mutation testing did not complete"
	reasonBaseline  = "tests must pass before mutation testing"
	reasonErrored   = "mutants could not be run"
	survivorFormat  = "%s:%d %s: %s -> %s (%s)"
)

var ErrIncomplete = errors.New(reasonIncomplet)

type overlay struct {
	Replace map[string]string `json:"Replace"`
}

type target struct {
	mutant     Mutant
	src        []byte
	pkg        golang.Package
	dependents []string
	covered    bool
}

type engine struct {
	ws        gate.Workspace
	baselines map[string]time.Duration
	dir       string
	cache     string
}

type Report struct {
	Counts    map[Status]int
	Survivors []string
	Entries   []entry
}

func (r Report) Score() float64 {
	killed := r.Counts[StatusKilled] + r.Counts[StatusTimedOut]
	total := killed + r.Counts[StatusSurvived] + r.Counts[StatusNotCovered]
	if total == 0 {
		return percentScale
	}
	return float64(killed) * percentScale / float64(total)
}

func Check(ctx context.Context, ws gate.Workspace) gate.Outcome {
	rep, err := Run(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if err := rep.Save(ws.Report(ReportFile)); err != nil {
		return gate.Fail("cannot keep the mutation report: " + err.Error())
	}
	values := map[string]float64{ValueScore: rep.Score(), ValueMutants: float64(totalMutants(rep.Counts))}
	for s, n := range rep.Counts {
		values[string(s)] = float64(n)
	}
	return gate.Judge(rep.Score() >= ws.Policy.Thresholds.MutationScore, reasonScore, values, rep.Survivors)
}

func totalMutants(counts map[Status]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

func Run(ctx context.Context, ws gate.Workspace) (Report, error) {
	covered, err := golang.CoveredLines(ws)
	if err != nil {
		return Report{}, err
	}
	sandbox := ws.Report(sandboxDir)
	defer func() { _ = os.RemoveAll(sandbox) }()
	if rerr := os.RemoveAll(sandbox); rerr != nil {
		return Report{}, rerr
	}
	if cerr := source.CopyTracked(ctx, ws, sandbox); cerr != nil {
		return Report{}, fmt.Errorf("cannot stage a sandbox: %w", cerr)
	}
	ws.Src = sandbox
	pkgs, err := golang.List(ctx, ws)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = os.RemoveAll(ws.Report(cacheDir)) }()
	e := &engine{ws: ws, baselines: map[string]time.Duration{}, dir: ws.Report(mutantsDir), cache: cacheEnv + ws.Report(cacheDir)}
	if berr := e.measureBaselines(ctx, pkgs); berr != nil {
		return Report{}, berr
	}
	targets, err := e.targets(pkgs, covered)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = os.RemoveAll(e.dir) }()
	return e.execute(ctx, targets)
}

func (e *engine) measureBaselines(ctx context.Context, pkgs golang.Packages) error {
	for _, p := range pkgs {
		if !p.HasTests() {
			continue
		}
		started := time.Now()
		_, err := e.ws.RunEnv(ctx, []string{e.cache}, golang.GoBinary, "test", "-count=1", p.ImportPath)
		if err != nil {
			return fmt.Errorf("%s: %s: %s", reasonBaseline, p.ImportPath, gate.Tail(procexec.Output(err)))
		}
		e.baselines[p.ImportPath] = time.Since(started)
	}
	return nil
}

func (e *engine) targets(pkgs golang.Packages, covered map[string]map[int]bool) ([]target, error) {
	var out []target
	for _, p := range pkgs.Production() {
		deps := pkgs.Dependents(p.ImportPath)
		for _, name := range p.GoFiles {
			found, err := fileTargets(p, name, deps, covered[p.ImportPath+"/"+name])
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
	}
	return out, nil
}

func fileTargets(p golang.Package, name string, deps []string, lines map[int]bool) ([]target, error) {
	path := filepath.Join(p.Dir, name)
	var points []Mutant
	src, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		points, err = Points(path, src)
	}
	if err != nil {
		return nil, err
	}
	out := make([]target, 0, len(points))
	for _, m := range points {
		out = append(out, target{mutant: m, src: src, pkg: p, dependents: deps, covered: executed(lines, m.Line)})
	}
	return out, nil
}

func executed(lines map[int]bool, line int) bool {
	covered, known := lines[line]
	return covered || !known
}

func (e *engine) execute(ctx context.Context, targets []target) (Report, error) {
	if err := os.MkdirAll(e.dir, dirPerm); err != nil {
		return Report{}, err
	}
	statuses := make([]Status, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	p := &progress{log: e.ws.Log, total: len(targets), step: max(1, len(targets)/progressSteps), started: time.Now()}
	workers := max(minWorkers, runtime.NumCPU()/cpuShare)
	e.ws.Log.Info("mutation testing started", "mutants", len(targets), "workers", workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				statuses[i] = e.run(ctx, i, targets[i])
				p.done(statuses[i])
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrIncomplete, ctx.Err())
	}
	rep := e.report(targets, statuses)
	if n := rep.Counts[StatusErrored]; n > 0 {
		return Report{}, fmt.Errorf("%w: %d %s", ErrIncomplete, n, reasonErrored)
	}
	return rep, nil
}

type progress struct {
	mu       sync.Mutex
	log      *slog.Logger
	total    int
	step     int
	finished int
	killed   int
	started  time.Time
}

func (p *progress) done(s Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished++
	if s == StatusKilled || s == StatusTimedOut {
		p.killed++
	}
	if p.finished%p.step == 0 || p.finished == p.total {
		p.log.Info("mutation progress", "done", p.finished, "total", p.total, "killed", p.killed, "elapsed_seconds", time.Since(p.started).Seconds())
	}
}

type entry struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	Status   Status `json:"status"`
}

func (e *engine) report(targets []target, statuses []Status) Report {
	rep := Report{Counts: map[Status]int{}}
	for i, t := range targets {
		m := t.mutant
		rep.Counts[statuses[i]]++
		rep.Entries = append(rep.Entries, entry{File: e.ws.Rel(m.File), Line: m.Line, Operator: m.Operator, Original: m.Original, Mutated: m.Mutated, Status: statuses[i]})
		if statuses[i] == StatusSurvived || statuses[i] == StatusNotCovered {
			rep.Survivors = append(rep.Survivors, fmt.Sprintf(survivorFormat, e.ws.Rel(m.File), m.Line, m.Operator, snippet(m.Original), snippet(m.Mutated), statuses[i]))
		}
	}
	return rep
}

func (r Report) Save(path string) error {
	data, err := json.Marshal(r.Entries)
	if err == nil {
		err = os.WriteFile(path, data, filePerm)
	}
	return err
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > snippetLimit {
		return s[:snippetLimit] + "..."
	}
	return s
}

func (e *engine) run(ctx context.Context, id int, t target) Status {
	if !t.covered {
		return StatusNotCovered
	}
	dir := filepath.Join(e.dir, strconv.Itoa(id))
	defer func() { _ = os.RemoveAll(dir) }()
	ov := filepath.Join(dir, overlayFile)
	if err := stage(dir, ov, t); err != nil {
		e.ws.Log.Error("mutant staging failed", "file", t.mutant.File, "error", err.Error())
		return StatusErrored
	}
	status := StatusSurvived
	if t.pkg.HasTests() {
		status = e.test(ctx, ov, []string{t.pkg.ImportPath})
	}
	if status == StatusSurvived && len(t.dependents) > 0 {
		status = e.test(ctx, ov, t.dependents)
	}
	return status
}

func stage(dir, ov string, t target) error {
	mutated := filepath.Join(dir, filepath.Base(t.mutant.File))
	data, err := json.Marshal(overlay{Replace: map[string]string{t.mutant.File: mutated}})
	if err == nil {
		err = os.MkdirAll(dir, dirPerm)
	}
	if err == nil {
		err = os.WriteFile(mutated, t.mutant.Apply(t.src), filePerm)
	}
	if err == nil {
		err = os.WriteFile(ov, data, filePerm)
	}
	return err
}

func deadline(limit, floor time.Duration) time.Duration {
	return 2*limit + floor
}

func (e *engine) timeout(pkgs []string) time.Duration {
	longest := time.Duration(0)
	for _, p := range pkgs {
		longest = max(longest, e.baselines[p])
	}
	return max(e.ws.Policy.Limits.MutantFloor, longest*time.Duration(e.ws.Policy.Limits.MutantFactor))
}

func (e *engine) test(ctx context.Context, ov string, pkgs []string) Status {
	limit := e.timeout(pkgs)
	tctx, cancel := context.WithTimeout(ctx, deadline(limit, e.ws.Policy.Limits.MutantFloor))
	defer cancel()
	args := append([]string{"test", "-count=1", "-failfast", "-timeout=" + limit.String()}, pkgs...)
	res, err := e.ws.RunEnv(tctx, []string{overlayEnv + ov, e.cache}, golang.GoBinary, args...)
	out := res.Combined()
	switch {
	case err == nil:
		return StatusSurvived
	case strings.Contains(out, buildFailed) || strings.Contains(out, setupFailed):
		return StatusNotViable
	case tctx.Err() != nil || strings.Contains(out, testTimedOut):
		return StatusTimedOut
	case strings.Contains(out, testFailed):
		return StatusKilled
	default:
		e.ws.Log.Error("mutant could not be run", "packages", pkgs, "output", gate.Tail(out))
		return StatusErrored
	}
}
