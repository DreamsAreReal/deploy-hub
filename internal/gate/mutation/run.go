package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	StatusExhausted  Status = "out-of-memory"
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
	countOnce       = "-count=1"
	goTest          = "test"
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
	key        string
	verdict    string
	cached     Status
}

type engine struct {
	ws        gate.Workspace
	baselines map[string]time.Duration
	dir       string
	cache     string
	cacheDir  string
	workers   []*workerCache
	index     *testIndex
}

type Report struct {
	Cached    int
	Counts    map[Status]int
	Survivors []string
	Entries   []entry
}

func (r Report) Score() float64 {
	killed := r.Counts[StatusKilled] + r.Counts[StatusTimedOut] + r.Counts[StatusExhausted]
	total := killed + r.Counts[StatusSurvived] + r.Counts[StatusNotCovered]
	if total == 0 {
		return percentScale
	}
	return float64(killed) * percentScale / float64(total)
}

func Check(ctx context.Context, ws gate.Workspace) gate.Outcome {
	rep, err := Run(ctx, ws)
	return Judge(ws, rep, err)
}

func Judge(ws gate.Workspace, rep Report, err error) gate.Outcome {
	if err != nil {
		return gate.Fail(err.Error())
	}
	if err := rep.Save(ws.Report(ReportFile)); err != nil {
		return gate.Fail("cannot keep the mutation report: " + err.Error())
	}
	values := map[string]float64{ValueScore: rep.Score(), ValueMutants: float64(totalMutants(rep.Counts)), ValueCached: float64(rep.Cached), ValueRun: float64(totalMutants(rep.Counts) - rep.Cached)}
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
	guard := newDiskGuard(ws)
	if err := guard.check(); err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrIncomplete, err)
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go guard.watch(ctx, stop, diskEvery)
	sandbox := ws.Report(sandboxDir)
	defer func() { _ = os.RemoveAll(sandbox) }()
	if rerr := os.RemoveAll(sandbox); rerr != nil {
		return Report{}, rerr
	}
	if cerr := source.CopyTracked(ctx, ws, sandbox); cerr != nil {
		return Report{}, fmt.Errorf("cannot stage a sandbox: %w", cerr)
	}
	ws.Src = sandbox
	ws.MemoryLimit = MemoryBudget()
	pkgs, err := golang.List(ctx, ws)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = os.RemoveAll(ws.Report(cacheDir)) }()
	e := &engine{ws: ws, baselines: map[string]time.Duration{}, dir: ws.Report(mutantsDir), cacheDir: ws.Report(cacheDir), cache: cacheEnv + ws.Report(cacheDir)}
	if berr := e.measureBaselines(ctx, pkgs); berr != nil {
		return Report{}, berr
	}
	return e.incremental(ctx, pkgs)
}

func (e *engine) measureBaselines(ctx context.Context, pkgs golang.Packages) error {
	if err := os.MkdirAll(e.ws.Report(testLogDir), dirPerm); err != nil {
		return err
	}
	for _, p := range pkgs {
		if !p.HasTests() {
			continue
		}
		started := time.Now()
		_, err := e.ws.RunEnv(ctx, []string{e.cache}, golang.GoBinary, goTest, countOnce, p.ImportPath, argsFlag, testLogFlag+e.testLog(p.ImportPath))
		if err != nil {
			return fmt.Errorf("%s: %s: %s", reasonBaseline, p.ImportPath, gate.Tail(procexec.Output(err)))
		}
		e.baselines[p.ImportPath] = time.Since(started)
	}
	return nil
}

func (e *engine) targets(pkgs golang.Packages) ([]target, error) {
	var out []target
	for _, p := range pkgs.Production() {
		deps := pkgs.Dependents(p.ImportPath)
		for _, name := range p.GoFiles {
			found, err := fileTargets(p, name, deps)
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
	}
	return out, nil
}

func fileTargets(p golang.Package, name string, deps []string) ([]target, error) {
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
		out = append(out, target{mutant: m, src: src, pkg: p, dependents: deps, key: p.ImportPath + "/" + name})
	}
	return out, nil
}

func (e *engine) execute(ctx context.Context, targets []target) (Report, error) {
	if err := os.MkdirAll(e.dir, dirPerm); err != nil {
		return Report{}, err
	}
	jobs := make([]Job, 0, len(targets))
	for i, t := range targets {
		jobs = append(jobs, Job{Mutant: t.mutant, Cached: t.cached, Run: func(ctx context.Context, w int) Status { return e.run(ctx, i, t, e.workers[w]) }})
	}
	return Execute(ctx, e.ws, jobs, Workers())
}

type Job struct {
	Mutant Mutant
	Cached Status
	Run    func(ctx context.Context, worker int) Status
}

func Workers() int {
	return max(minWorkers, procexec.CPUs()/cpuShare)
}

func MemoryBudget() int64 {
	return procexec.TotalMemory() / int64(Workers()+1)
}

func (s Status) Detected() bool {
	return s == StatusKilled || s == StatusTimedOut || s == StatusExhausted
}

func Execute(ctx context.Context, ws gate.Workspace, jobs []Job, workers int) (Report, error) {
	statuses := make([]Status, len(jobs))
	queue := make(chan int)
	var wg sync.WaitGroup
	p := &progress{log: ws.Log, total: len(jobs), step: max(1, len(jobs)/progressSteps), started: time.Now()}
	ws.Log.Info("mutation testing started", "mutants", len(jobs), "workers", workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				statuses[i] = jobs[i].status(ctx, w)
				p.done(statuses[i])
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	wg.Wait()
	rep := report(ws, jobs, statuses)
	if ctx.Err() != nil {
		return rep, fmt.Errorf("%w: %w", ErrIncomplete, context.Cause(ctx))
	}
	if n := rep.Counts[StatusErrored]; n > 0 {
		return rep, fmt.Errorf("%w: %d %s", ErrIncomplete, n, reasonErrored)
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
	if s.Detected() {
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
	Source   string `json:"source"`
}

func report(ws gate.Workspace, jobs []Job, statuses []Status) Report {
	rep := Report{Counts: map[Status]int{}}
	for i, j := range jobs {
		m := j.Mutant
		rep.Counts[statuses[i]]++
		source := SourceRun
		if j.Cached != "" {
			rep.Cached++
			source = SourceCache
		}
		rep.Entries = append(rep.Entries, entry{File: ws.Rel(m.File), Line: m.Line, Operator: m.Operator, Original: m.Original, Mutated: m.Mutated, Status: statuses[i], Source: source})
		if statuses[i] == StatusSurvived || statuses[i] == StatusNotCovered {
			rep.Survivors = append(rep.Survivors, fmt.Sprintf(survivorFormat, ws.Rel(m.File), m.Line, m.Operator, snippet(m.Original), snippet(m.Mutated), statuses[i]))
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

func (e *engine) run(ctx context.Context, id int, t target, c *workerCache) Status {
	status := e.mutate(ctx, id, t, cacheEnv+c.dir)
	if err := c.trim(); err != nil {
		e.ws.Log.Error("worker build cache trim failed", "dir", c.dir, "error", err.Error())
		return StatusErrored
	}
	return status
}

func (e *engine) mutate(ctx context.Context, id int, t target, cache string) Status {
	sel := e.index.selectFor(t.key, t.mutant.Line, append([]string{t.pkg.ImportPath}, t.dependents...))
	if sel.none() {
		return StatusNotCovered
	}
	dir := filepath.Join(e.dir, strconv.Itoa(id))
	defer func() { _ = os.RemoveAll(dir) }()
	ov := filepath.Join(dir, overlayFile)
	if err := stage(dir, ov, t); err != nil {
		e.ws.Log.Error("mutant staging failed", "file", t.mutant.File, "error", err.Error())
		return StatusErrored
	}
	m := mutantRun{overlay: ov, cache: cache}
	if sel.full {
		return e.runWhole(ctx, m, t)
	}
	return e.runSelected(ctx, m, sel)
}

func (e *engine) runWhole(ctx context.Context, m mutantRun, t target) Status {
	status := StatusSurvived
	if t.pkg.HasTests() {
		status = e.test(ctx, m, []string{t.pkg.ImportPath})
	}
	if status == StatusSurvived && len(t.dependents) > 0 {
		status = e.test(ctx, m, t.dependents)
	}
	return status
}

func (e *engine) runSelected(ctx context.Context, m mutantRun, sel selection) Status {
	status := StatusSurvived
	if len(sel.tests) > 0 {
		pkgs, run := selectedArgs(sel.tests)
		status = e.test(ctx, m, pkgs, run...)
	}
	if status == StatusSurvived && len(sel.whole) > 0 {
		status = e.test(ctx, m, sel.whole)
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

type mutantRun struct {
	overlay string
	cache   string
}

func (e *engine) test(ctx context.Context, m mutantRun, pkgs []string, extra ...string) Status {
	limit := e.timeout(pkgs)
	tctx, cancel := context.WithTimeout(ctx, deadline(limit, e.ws.Policy.Limits.MutantFloor))
	defer cancel()
	args := append(append([]string{goTest, countOnce, failFast, "-timeout=" + limit.String()}, extra...), pkgs...)
	res, err := e.ws.RunEnv(tctx, []string{overlayEnv + m.overlay, m.cache}, golang.GoBinary, args...)
	out := res.Combined()
	switch {
	case err == nil:
		return StatusSurvived
	case strings.Contains(out, buildFailed) || strings.Contains(out, setupFailed):
		return StatusNotViable
	case errors.Is(err, procexec.ErrMemoryLimit):
		return StatusExhausted
	case tctx.Err() != nil || strings.Contains(out, testTimedOut):
		return StatusTimedOut
	case strings.Contains(out, testFailed):
		return StatusKilled
	default:
		e.ws.Log.Error("mutant could not be run", "packages", pkgs, "output", gate.Tail(out))
		return StatusErrored
	}
}
