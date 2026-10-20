package mutation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	exLess    = "a < b"
	exGE      = "a >= b"
	exLE      = "i <= b"
	exGreater = "a > b"
	exAnd     = "a < b && !(a == 0)"
	exOr      = "a > b || a != b"
	exMul     = "a*b"
	exDiv     = "a/b"
	exLEab    = "a <= b"
	exInc     = "n + 1"
	exConcat  = `"x" + "y"`
	fileLib   = "lib.go"
	fileX     = "x.go"
	outOK     = "ok"
	pkgLib    = "m/lib"
	pkgApp    = "m/app"
	srcMarker = "SRC"
)

const sample = `package lib

import "errors"

var errBad = errors.New("bad")

func Compare(a, b int) bool {
	if a < b && !(a == 0) {
		return true
	}
	for i := 0; i <= b; i++ {
		a += i
	}
	return a > b || a != b
}

func Math(a, b int) (int, string, error) {
	n := a*b - a/b + a%b
	n++
	use(n)
	if err := check(n); err != nil {
		return 0, "", err
	}
	return n + 1, "x" + "y", nil
}

func Ptr(p *int, m map[string]int, s []int, f func(), a [2]int, i any) (*int, map[string]int, []int, func(), [2]int, any, float64, Named) {
	return p, m, s, f, a, i, 0, Named{}
}

func Nils() (*int, error, bool, bool) {
	return nil, errNone, false, true
}

func Literal() func() int {
	return func() int { return 2 }
}

func (Named) Method() int { return 0 }

func Bare() (n int) {
	return
}

func Multi() (int, error) {
	return pair()
}

func Loop(ch chan int) chan int {
	for {
		if len(ch) > 0 {
			return ch
		}
	}
}

func NoResult() {
	return
}

var errNone error

func use(int)                {}
func check(int) error        { return nil }
func pair() (int, error)     { return 0, nil }
type Named struct{}
`

func points(t *testing.T) []Mutant {
	t.Helper()
	ms, err := Points(fileLib, []byte(sample))
	testutil.NoErr(t, err)
	return ms
}

func find(ms []Mutant, op, original string) []string {
	var out []string
	for _, m := range ms {
		if m.Operator == op && m.Original == original {
			out = append(out, m.Mutated)
		}
	}
	return out
}

func TestPointsCoverEveryMandatoryOperator(t *testing.T) {
	ms := points(t)
	cases := []struct {
		op, original string
		want         []string
	}{
		{OpBoundary, exLess, []string{exLEab}},
		{OpComparison, exLess, []string{exGE}},
		{OpBoundary, exLE, []string{"i < b"}},
		{OpComparison, exLE, []string{"i > b"}},
		{OpComparison, "a == 0", []string{"a != 0"}},
		{OpComparison, exGreater, []string{exLEab}},
		{OpBoundary, exGreater, []string{exGE}},
		{OpComparison, "a != b", []string{"a == b"}},
		{OpLogical, exAnd, []string{"a < b || !(a == 0)"}},
		{OpLogical, exOr, []string{"a > b && a != b"}},
		{OpRemoveNegation, "!(a == 0)", []string{"(a == 0)"}},
		{OpNegateCondition, exAnd, []string{"!(a < b && !(a == 0))"}},
		{OpNegateCondition, exLE, []string{"!(i <= b)"}},
		{OpArithmetic, "a += i", []string{"a -= i"}},
		{OpArithmetic, "a*b - a/b + a%b", []string{"a*b - a/b - a%b"}},
		{OpArithmetic, "a*b - a/b", []string{"a*b + a/b"}},
		{OpArithmetic, "a * b", nil},
		{OpArithmetic, exMul, []string{exDiv}},
		{OpArithmetic, exDiv, []string{exMul}},
		{OpArithmetic, "a%b", []string{exMul}},
		{OpArithmetic, exInc, []string{"n - 1"}},
		{OpIncDec, "i++", []string{"i--"}},
		{OpIncDec, "n++", []string{"n--"}},
		{OpRemoveStatement, "use(n)", []string{""}},
		{OpErrorHandling, "err != nil", []string{identFalse}},
		{OpReturnValue, identTrue, []string{identFalse, identFalse}},
		{OpReturnValue, identFalse, []string{identTrue}},
		{OpReturnValue, exOr, []string{"!(a > b || a != b)"}},
		{OpReturnValue, "err", []string{identNil}},
		{OpReturnValue, `""`, []string{`("") + "mutated"`}},
		{OpReturnValue, "0", slices.Repeat([]string{"(0) + 1"}, 4)},
		{OpReturnValue, exInc, []string{"(n + 1) + 1"}},
		{OpReturnValue, "p", []string{identNil}},
		{OpReturnValue, "m", []string{identNil}},
		{OpReturnValue, "s", []string{identNil}},
		{OpReturnValue, "f", []string{identNil}},
		{OpReturnValue, "i", []string{identNil}},
		{OpReturnValue, "func() int { return 2 }", []string{identNil}},
		{OpReturnValue, "2", []string{"(2) + 1"}},
		{OpReturnValue, "a", nil},
		{OpReturnValue, "Named{}", nil},
		{OpReturnValue, identNil, nil},
		{OpReturnValue, "errNone", []string{identNil}},
		{OpReturnValue, "ch", []string{identNil}},
		{OpReturnValue, exConcat, []string{`("x" + "y") + "mutated"`}},
		{OpArithmetic, exConcat, nil},
	}
	for _, c := range cases {
		testutil.Equal(t, find(ms, c.op, c.original), c.want)
	}
	for _, m := range ms {
		testutil.True(t, m.Line > 0 && m.File == fileLib, "position recorded")
	}
}

func TestApplyRewritesOnlyTheMutatedRange(t *testing.T) {
	ms := points(t)
	for _, m := range ms {
		if m.Operator == OpNegateCondition && m.Original == "i <= b" {
			out := string(m.Apply([]byte(sample)))
			testutil.Contains(t, out, "for i := 0; !(i <= b); i++ {")
			testutil.Equal(t, strings.Count(out, "!("), strings.Count(sample, "!(")+1)
		}
	}
	src := []byte(sample)
	_ = ms[0].Apply(src)
	testutil.Equal(t, string(src), sample)
	_, err := Points("bad.go", []byte("package x\nfunc {"))
	testutil.Fails(t, err)
}

func TestScore(t *testing.T) {
	testutil.Equal(t, Report{Counts: map[Status]int{}}.Score(), float64(percentScale))
	r := Report{Counts: map[Status]int{StatusKilled: 5, StatusTimedOut: 3, StatusExhausted: 1, StatusSurvived: 0, StatusNotCovered: 1, StatusNotViable: 50}}
	testutil.Equal(t, r.Score(), float64(90))
	testutil.Equal(t, totalMutants(r.Counts), 60)
	testutil.True(t, StatusExhausted.Detected() && StatusTimedOut.Detected() && !StatusSurvived.Detected(), "detected statuses")
	testutil.Equal(t, snippet("a\n\t b"), "a b")
	testutil.Equal(t, len(snippet(strings.Repeat("x", snippetLimit+5))), snippetLimit+len("..."))
	testutil.Equal(t, snippet(strings.Repeat("y", snippetLimit)), strings.Repeat("y", snippetLimit))
	mixed := Report{Counts: map[Status]int{StatusKilled: 8, StatusSurvived: 1, StatusNotCovered: 1}}
	testutil.Equal(t, mixed.Score(), float64(80))
}

type outcome struct {
	killed    bool
	notViable bool
	timeout   bool
	broken    bool
	exhausted bool
}

type fakeGo struct {
	t        *testing.T
	pkgs     string
	baseErr  error
	mu       sync.Mutex
	decide   func(mutated string, pkgs []string) outcome
	overlays []string
	caches   []string
	limit    int64
}

func (f *fakeGo) handle(c procexec.Command) (procexec.Result, error) {
	switch {
	case c.Name == source.GitBinary:
		return procexec.Result{Stdout: "lib/" + fileLib + "\x00"}, nil
	case c.Args[0] == "list":
		return procexec.Result{Stdout: strings.ReplaceAll(f.pkgs, srcMarker, c.Dir)}, nil
	case !strings.HasPrefix(c.Env[0], overlayEnv):
		testutil.Mkdir(f.t, strings.TrimPrefix(c.Env[0], cacheEnv))
		return procexec.Result{Stdout: outOK}, f.baseErr
	}
	ov := strings.TrimPrefix(c.Env[0], overlayEnv)
	var o overlay
	testutil.NoErr(f.t, json.Unmarshal([]byte(testutil.Read(f.t, ov)), &o))
	var mutated string
	for _, path := range o.Replace {
		mutated = testutil.Read(f.t, path)
	}
	f.mu.Lock()
	f.overlays = append(f.overlays, ov)
	f.caches = append(f.caches, c.Env[1:]...)
	f.limit = c.MemoryLimit
	f.mu.Unlock()
	res := f.decide(mutated, c.Args[4:])
	switch {
	case res.notViable:
		return procexec.Result{Stdout: "FAIL lib [build failed]"}, errors.New("exit 1")
	case res.timeout:
		return procexec.Result{Stdout: "panic: test timed out after 1s"}, errors.New("exit 1")
	case res.killed:
		return procexec.Result{Stdout: "--- FAIL: TestX"}, errors.New("exit 1")
	case res.broken:
		return procexec.Result{Stderr: "go: creating work dir: no space left on device"}, errors.New("exit 1")
	case res.exhausted:
		return procexec.Result{}, fmt.Errorf("%w: signal: killed", procexec.ErrMemoryLimit)
	}
	return procexec.Result{Stdout: outOK}, nil
}

const tiny = `package lib

func Pos(a int) bool {
	return a > 0
}

func Neg(a int) bool {
	return a < 0
}
`

func setup(t *testing.T, decide func(string, []string) outcome) (gate.Workspace, *fakeGo) {
	t.Helper()
	src := t.TempDir()
	dir := filepath.Join(src, "lib")
	testutil.Write(t, filepath.Join(dir, fileLib), tiny)
	pkgs := []golang.Package{
		{ImportPath: pkgLib, Name: "lib", Dir: srcMarker + "/lib", GoFiles: []string{fileLib}, TestGoFiles: []string{"lib_test.go"}},
		{ImportPath: pkgApp, Name: "app", Dir: srcMarker, GoFiles: []string{"main.go"}, XTestGoFiles: []string{"app_test.go"}, Deps: []string{pkgLib}},
	}
	var listing strings.Builder
	for _, p := range pkgs[:1] {
		data, err := json.Marshal(p)
		testutil.NoErr(t, err)
		listing.Write(data)
	}
	fg := &fakeGo{t: t, pkgs: listing.String(), decide: decide}
	out := t.TempDir()
	testutil.Write(t, filepath.Join(out, golang.CoverReport), "mode: atomic\nm/lib/lib.go:3.22,5.2 1 1\nm/lib/lib.go:7.22,9.2 1 0\n")
	ws := gate.Workspace{Src: src, Out: out, Exec: &procexectest.Fake{Handler: fg.handle}, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{Thresholds: policy.Thresholds{MutationScore: 50}, Limits: policy.Limits{MutantFloor: time.Second, MutantFactor: 2}}}
	return ws, fg
}

func TestRunClassifiesMutants(t *testing.T) {
	ws, fg := setup(t, func(mutated string, _ []string) outcome {
		switch {
		case strings.Contains(mutated, "a >= 0"):
			return outcome{notViable: true}
		case strings.Contains(mutated, "a <= 0"):
			return outcome{timeout: true}
		case strings.Contains(mutated, "!(a > 0)"):
			return outcome{killed: true}
		}
		return outcome{}
	})
	rep, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[Status]int{StatusNotViable: 1, StatusTimedOut: 1, StatusKilled: 1, StatusNotCovered: 3})
	testutil.Equal(t, len(rep.Survivors), 3)
	testutil.Contains(t, strings.Join(rep.Survivors, "\n"), "lib/lib.go:8 comparison-boundary: a < 0 -> a <= 0 (not-covered)")
	testutil.Equal(t, rep.Score(), float64(40))
	testutil.Equal(t, len(fg.overlays), 3)
	_, err = os.Stat(filepath.Join(ws.Out, mutantsDir))
	testutil.True(t, os.IsNotExist(err), "mutant files are removed")
	out := Check(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[ValueScore], float64(40))
	testutil.Equal(t, out.Values[string(StatusKilled)], float64(1))
	testutil.Equal(t, out.Values[ValueMutants], float64(6))
}

func TestSurvivorsAreRetriedWithDependentPackages(t *testing.T) {
	ws, _ := setup(t, func(_ string, pkgs []string) outcome {
		return outcome{killed: len(pkgs) == 1 && pkgs[0] == pkgApp}
	})
	pkgs := `{"ImportPath":"m/lib","Name":"lib","Dir":"` + srcMarker + `/lib","GoFiles":["lib.go"],"TestGoFiles":["lib_test.go"]}
{"ImportPath":"m/app","Name":"app","Dir":"` + srcMarker + `","XTestGoFiles":["app_test.go"],"Deps":["m/lib"]}`
	ws.Exec = &procexectest.Fake{Handler: (&fakeGo{t: t, pkgs: pkgs, decide: func(_ string, p []string) outcome {
		return outcome{killed: p[0] == pkgApp}
	}}).handle}
	ws.Policy.Thresholds.MutationScore = 40
	rep, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[StatusKilled], 3)
	testutil.Equal(t, Check(context.Background(), ws).Status, gate.StatusPass)
}

func TestRunFailures(t *testing.T) {
	ws, fg := setup(t, func(string, []string) outcome { return outcome{} })
	fg.baseErr = errors.New("tests red")
	_, err := Run(context.Background(), ws)
	testutil.Contains(t, err.Error(), reasonBaseline)
	testutil.Contains(t, Check(context.Background(), ws).Reason, reasonBaseline)
	noCover, _ := setup(t, func(string, []string) outcome { return outcome{} })
	testutil.NoErr(t, os.Remove(filepath.Join(noCover.Out, golang.CoverReport)))
	_, err = Run(context.Background(), noCover)
	testutil.Fails(t, err)
	noList, _ := setup(t, nil)
	noList.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("no go") }}
	_, err = Run(context.Background(), noList)
	testutil.Fails(t, err)
	badSrc, _ := setup(t, func(string, []string) outcome { return outcome{} })
	testutil.Write(t, filepath.Join(badSrc.Src, "lib", fileLib), "package lib\nfunc {")
	_, err = Run(context.Background(), badSrc)
	testutil.Fails(t, err)
	gone, _ := setup(t, func(string, []string) outcome { return outcome{} })
	testutil.NoErr(t, os.Remove(filepath.Join(gone.Src, "lib", fileLib)))
	_, err = Run(context.Background(), gone)
	testutil.Fails(t, err)
	blocked, _ := setup(t, func(string, []string) outcome { return outcome{} })
	testutil.Write(t, filepath.Join(blocked.Out, mutantsDir), "file in the way")
	_, err = Run(context.Background(), blocked)
	testutil.Fails(t, err)
}

func TestBrokenToolchainMakesTheRunIncomplete(t *testing.T) {
	ws, fg := setup(t, func(mutated string, _ []string) outcome {
		return outcome{killed: !strings.Contains(mutated, "a >= 0"), broken: true}
	})
	var logs bytes.Buffer
	ws.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	_, err := Run(context.Background(), ws)
	testutil.Is(t, err, ErrIncomplete)
	testutil.Contains(t, err.Error(), "1 "+reasonErrored)
	testutil.Contains(t, logs.String(), `"msg":"mutant could not be run","packages":["m/lib"],"output":"go: creating work dir: no space left on device"`)
	testutil.True(t, len(fg.caches) > 0, "mutants run with the isolated build cache")
	for _, c := range fg.caches {
		testutil.Equal(t, c, cacheEnv+filepath.Join(ws.Out, cacheDir))
	}
	testutil.Missing(t, filepath.Join(ws.Out, cacheDir))
}

func TestCancelledRunIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ws, _ := setup(t, func(string, []string) outcome {
		cancel()
		return outcome{}
	})
	_, err := Run(ctx, ws)
	testutil.Is(t, err, ErrIncomplete)
}

func TestStagingFailureIsAnError(t *testing.T) {
	ws, _ := setup(t, func(string, []string) outcome { return outcome{killed: true} })
	var logs bytes.Buffer
	ws.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	e := &engine{ws: ws, baselines: map[string]time.Duration{}, dir: filepath.Join(ws.Out, "missing", "\x00")}
	st := e.run(context.Background(), 1, target{mutant: Mutant{File: fileX}, covered: true})
	testutil.Equal(t, st, StatusErrored)
	testutil.Contains(t, logs.String(), `"msg":"mutant staging failed","file":"x.go"`)
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "7"), "file blocks the mutant dir")
	testutil.Fails(t, stage(filepath.Join(dir, "7"), filepath.Join(dir, "7", overlayFile), target{mutant: Mutant{File: fileX}}))
	testutil.Mkdir(t, filepath.Join(dir, "8", fileX))
	testutil.Fails(t, stage(filepath.Join(dir, "8"), filepath.Join(dir, "8", overlayFile), target{mutant: Mutant{File: fileX}}))
	testutil.Mkdir(t, filepath.Join(dir, "9", overlayFile))
	testutil.Fails(t, stage(filepath.Join(dir, "9"), filepath.Join(dir, "9", overlayFile), target{mutant: Mutant{File: fileX}}))
	testutil.Equal(t, deadline(3*time.Second, time.Second), 7*time.Second)
}

func TestTimeoutScalesWithBaseline(t *testing.T) {
	ws, _ := setup(t, nil)
	e := &engine{ws: ws, baselines: map[string]time.Duration{"a": 3 * time.Second, "b": time.Millisecond}}
	testutil.Equal(t, e.timeout([]string{"a", "b"}), 6*time.Second)
	testutil.Equal(t, e.timeout([]string{"b"}), time.Second)
}

func TestRunCountsMemoryExhaustionAsDetected(t *testing.T) {
	ws, fg := setup(t, func(mutated string, _ []string) outcome {
		hungry := strings.Contains(mutated, "a >= 0")
		return outcome{exhausted: hungry, killed: !hungry}
	})
	rep, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[StatusExhausted], 1)
	testutil.Equal(t, rep.Counts[StatusKilled], 2)
	testutil.Equal(t, fg.limit, MemoryBudget())
	testutil.Equal(t, MemoryBudget(), procexec.TotalMemory()/int64(Workers()+1))
}

func TestProgressLogs(t *testing.T) {
	var b strings.Builder
	p := &progress{log: slog.New(slog.NewTextHandler(&b, nil)), total: 4, step: 2, started: time.Now()}
	for _, s := range []Status{StatusKilled, StatusKilled, StatusSurvived, StatusTimedOut} {
		p.done(s)
	}
	testutil.Equal(t, strings.Count(b.String(), "mutation progress"), 2)
	testutil.Contains(t, b.String(), "done=2 total=4 killed=2", "done=4 total=4 killed=3")
}

func FuzzPoints(f *testing.F) {
	f.Add([]byte(sample))
	f.Fuzz(func(t *testing.T, src []byte) {
		ms, err := Points("f.go", src)
		if err != nil {
			return
		}
		for _, m := range ms {
			if len(m.Apply(src)) == 0 && len(src) > 0 && m.Operator != OpRemoveStatement {
				t.Fatalf("mutant erased the file: %+v", m)
			}
		}
	})
}

func TestCheckKeepsTheFullReport(t *testing.T) {
	ws, _ := setup(t, func(string, []string) outcome { return outcome{killed: true} })
	out := Check(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	var entries []entry
	testutil.NoErr(t, json.Unmarshal([]byte(testutil.Read(t, filepath.Join(ws.Out, ReportFile))), &entries))
	testutil.Equal(t, len(entries), 6)
	testutil.Equal(t, entries[0].File, "lib/lib.go")
	blocked, _ := setup(t, func(string, []string) outcome { return outcome{killed: true} })
	testutil.Mkdir(t, filepath.Join(blocked.Out, ReportFile))
	testutil.Contains(t, Check(context.Background(), blocked).Reason, "cannot keep the mutation report")
}

func TestMutantsRunInASandbox(t *testing.T) {
	var (
		mu   sync.Mutex
		dirs []string
	)
	ws, _ := setup(t, nil)
	fg := &fakeGo{t: t, pkgs: strings.ReplaceAll(`{"ImportPath":"m/lib","Name":"lib","Dir":"SRC/lib","GoFiles":["lib.go"],"TestGoFiles":["lib_test.go"]}`, "SRC", srcMarker),
		decide: func(string, []string) outcome { return outcome{killed: true} }}
	ws.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Name == golang.GoBinary && c.Args[0] == "test" {
			mu.Lock()
			dirs = append(dirs, c.Dir)
			mu.Unlock()
		}
		return fg.handle(c)
	}}
	_, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	for _, d := range dirs {
		testutil.Equal(t, d, filepath.Join(ws.Out, sandboxDir))
	}
	testutil.Missing(t, filepath.Join(ws.Out, sandboxDir))
	testutil.Equal(t, testutil.Read(t, filepath.Join(ws.Src, "lib", fileLib)), tiny)
	gone, _ := setup(t, func(string, []string) outcome { return outcome{} })
	testutil.NoErr(t, os.Remove(filepath.Join(gone.Src, "lib", fileLib)))
	_, err = Run(context.Background(), gone)
	testutil.Contains(t, err.Error(), "cannot stage a sandbox")
}

func TestKilledMutantsAreNotRetriedAndProgressIsLogged(t *testing.T) {
	ws, _ := setup(t, nil)
	var (
		mu    sync.Mutex
		calls [][]string
		logs  bytes.Buffer
	)
	ws.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	pkgs := `{"ImportPath":"m/lib","Name":"lib","Dir":"` + srcMarker + `/lib","GoFiles":["lib.go"],"TestGoFiles":["lib_test.go"]}
{"ImportPath":"m/app","Name":"app","Dir":"` + srcMarker + `","XTestGoFiles":["app_test.go"],"Deps":["m/lib"]}`
	fg := &fakeGo{t: t, pkgs: pkgs, decide: func(_ string, p []string) outcome {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
		return outcome{killed: p[0] == pkgLib}
	}}
	ws.Exec = &procexectest.Fake{Handler: fg.handle}
	rep, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[Status]int{StatusKilled: 3, StatusNotCovered: 3})
	testutil.Equal(t, len(calls), 3)
	for _, c := range calls {
		testutil.Equal(t, c, []string{pkgLib})
	}
	testutil.Contains(t, logs.String(), fmt.Sprintf(`"msg":"mutation testing started","mutants":6,"workers":%d`, max(minWorkers, runtime.NumCPU()/cpuShare)))
	testutil.Equal(t, strings.Count(logs.String(), `"msg":"mutation progress"`), 6)
	testutil.True(t, executed(map[int]bool{}, 3) && executed(map[int]bool{3: true}, 3) && !executed(map[int]bool{3: false}, 3), "unknown lines are executed")
}

func TestExecuteHandsWorkersTheirIndex(t *testing.T) {
	var (
		mu   sync.Mutex
		seen = map[int]bool{}
	)
	ws := gate.Workspace{Src: "/src", Log: slog.New(slog.DiscardHandler)}
	jobs := make([]Job, 0, 8)
	for i := range 8 {
		jobs = append(jobs, Job{Mutant: NewMutant("/src/a.py", i+1, OpArithmetic, "a + b", 10, Edit{Offset: 12, Length: 1, Text: "-"}), Run: func(_ context.Context, worker int) Status {
			mu.Lock()
			seen[worker] = true
			mu.Unlock()
			return StatusKilled
		}})
	}
	rep, err := Execute(context.Background(), ws, jobs, 3)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[StatusKilled], 8)
	for w := range seen {
		testutil.True(t, w >= 0 && w < 3, "worker index in range")
	}
	testutil.Equal(t, rep.Entries[0], entry{File: "a.py", Line: 1, Operator: OpArithmetic, Original: "a + b", Mutated: "a - b", Status: StatusKilled})
	testutil.Equal(t, Workers(), max(minWorkers, runtime.NumCPU()/cpuShare))
	out := Judge(gate.Workspace{Out: t.TempDir(), Policy: policy.Policy{Thresholds: policy.Thresholds{MutationScore: 90}}}, rep, nil)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, Judge(ws, Report{}, ErrIncomplete).Reason, reasonIncomplet)
}
