package mutation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	indexDir      = "test-index"
	profileExt    = ".out"
	runPattern    = "-run=^(%s)$"
	runSeparator  = "|"
	coverModeSet  = "-covermode=set"
	coverPkgFlag  = "-coverpkg="
	coverProfile  = "-coverprofile="
	listFailed    = "go test -list %s: %w: %s"
	profileFailed = "test index profile %s: %w"
)

var testName = regexp.MustCompile(`^(Test|Fuzz|Example)\w*$`)

type testRef struct {
	pkg  string
	name string
}

type testIndex struct {
	mu           sync.Mutex
	covers       map[string]map[int][]testRef
	instrumented map[string]map[int]bool
	whole        map[string]bool
}

type selection struct {
	full  bool
	tests []testRef
	whole []string
}

func (s selection) none() bool {
	return !s.full && len(s.tests) == 0 && len(s.whole) == 0
}

func newIndex() *testIndex {
	return &testIndex{covers: map[string]map[int][]testRef{}, instrumented: map[string]map[int]bool{}, whole: map[string]bool{}}
}

func (ix *testIndex) selectFor(file string, line int, pkgs []string) selection {
	if !ix.instrumented[file][line] {
		return selection{full: true}
	}
	var whole []string
	for _, p := range pkgs {
		if ix.whole[p] {
			whole = append(whole, p)
		}
	}
	var tests []testRef
	for _, r := range ix.covers[file][line] {
		if !ix.whole[r.pkg] {
			tests = append(tests, r)
		}
	}
	return selection{tests: tests, whole: whole}
}

func (ix *testIndex) add(ref testRef, lines map[string]map[int]bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for file, states := range lines {
		ix.mark(file, states, ref)
	}
}

func (ix *testIndex) mark(file string, states map[int]bool, ref testRef) {
	if ix.instrumented[file] == nil {
		ix.instrumented[file] = map[int]bool{}
		ix.covers[file] = map[int][]testRef{}
	}
	for line, covered := range states {
		ix.instrumented[file][line] = true
		if covered {
			ix.covers[file][line] = append(ix.covers[file][line], ref)
		}
	}
}

func (ix *testIndex) isolate(pkg string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.whole[pkg] = true
}

func (e *engine) buildIndex(ctx context.Context, pkgs golang.Packages, need map[string]bool) (*testIndex, error) {
	started := time.Now()
	refs, err := e.listTests(ctx, pkgs, need)
	if err != nil {
		return nil, err
	}
	dir := e.ws.Report(indexDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ix := newIndex()
	coverPkg := coverPkgFlag + strings.Join(pkgs.Production().Paths(), ",")
	jobs := make([]Job, 0, len(refs))
	for i, ref := range refs {
		profile := filepath.Join(dir, strconv.Itoa(i)+profileExt)
		jobs = append(jobs, Job{Run: func(ctx context.Context, _ int) Status { return e.indexTest(ctx, ix, ref, coverPkg, profile) }})
	}
	if err := runAll(ctx, jobs, Workers()); err != nil {
		return nil, err
	}
	e.ws.Log.Info("mutation test index built", "tests", len(refs), "isolated_packages", len(ix.whole), "seconds", time.Since(started).Seconds())
	return ix, nil
}

func (e *engine) listTests(ctx context.Context, pkgs golang.Packages, need map[string]bool) ([]testRef, error) {
	var refs []testRef
	for _, p := range pkgs {
		if !p.HasTests() || !need[p.ImportPath] {
			continue
		}
		res, err := e.ws.RunEnv(ctx, []string{e.cache}, golang.GoBinary, goTest, "-list", ".", p.ImportPath)
		if err != nil {
			return nil, fmt.Errorf(listFailed, p.ImportPath, err, gate.Tail(procexec.Output(err)))
		}
		refs = append(refs, listed(p.ImportPath, res.Stdout)...)
	}
	return refs, nil
}

func listed(pkg, out string) []testRef {
	var refs []testRef
	for _, l := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(l); testName.MatchString(name) {
			refs = append(refs, testRef{pkg: pkg, name: name})
		}
	}
	return refs
}

func (e *engine) indexTest(ctx context.Context, ix *testIndex, ref testRef, coverPkg, profile string) Status {
	args := []string{goTest, countOnce, coverModeSet, coverPkg, coverProfile + profile, fmt.Sprintf(runPattern, ref.name), ref.pkg}
	if _, err := e.ws.RunEnv(ctx, []string{e.cache}, golang.GoBinary, args...); err != nil {
		e.ws.Log.Info("test does not pass alone, its package runs whole", "package", ref.pkg, "test", ref.name)
		ix.isolate(ref.pkg)
		return StatusSurvived
	}
	lines, err := readProfile(profile)
	if err != nil {
		e.ws.Log.Error("test index profile unreadable", "package", ref.pkg, "test", ref.name, "error", err.Error())
		return StatusErrored
	}
	ix.add(ref, lines)
	return StatusSurvived
}

func readProfile(path string) (map[string]map[int]bool, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		var lines map[string]map[int]bool
		lines, err = golang.ProfileLines(string(data))
		if err == nil {
			return lines, nil
		}
	}
	return nil, fmt.Errorf(profileFailed, path, err)
}

func runAll(ctx context.Context, jobs []Job, workers int) error {
	statuses := make([]Status, len(jobs))
	queue := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range queue {
				statuses[i] = jobs[i].Run(ctx, 0)
			}
		})
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	wg.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ErrIncomplete, ctx.Err())
	}
	if slices.Contains(statuses, StatusErrored) {
		return fmt.Errorf("%w: the test index is incomplete", ErrIncomplete)
	}
	return nil
}

func selectedArgs(refs []testRef) ([]string, []string) {
	pkgs := make([]string, 0, len(refs))
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		pkgs = append(pkgs, r.pkg)
		names = append(names, r.name)
	}
	slices.Sort(pkgs)
	slices.Sort(names)
	return slices.Compact(pkgs), []string{fmt.Sprintf(runPattern, strings.Join(slices.Compact(names), runSeparator))}
}
