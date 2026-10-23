package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	testLogDir     = "testlog"
	testLogFlag    = "-test.testlogfile="
	testLogExt     = ".log"
	argsFlag       = "-args"
	failFast       = "-failfast"
	ValueCached    = "mutants_cached"
	ValueRun       = "mutants_run"
	envFailed      = "go env: %w: %s"
	logStaleCache  = "mutation verdict cache was built for another engine, policy, toolchain or module; every mutant runs"
	logCacheLoaded = "mutation verdict cache loaded"
)

var (
	engineFlags   = []string{countOnce, failFast, coverModeSet}
	toolchainVars = []string{"GOVERSION", "GOOS", "GOARCH", "GOAMD64", "GOARM64", "CGO_ENABLED", "GOFLAGS", "GOEXPERIMENT",
		"GOTOOLCHAIN", "GODEBUG", "CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS"}
	moduleFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum", "vendor/modules.txt"}
)

func (e *engine) keyBase(ctx context.Context) (KeyBase, error) {
	res, err := e.ws.RunEnv(ctx, []string{e.cache}, golang.GoBinary, append([]string{"env", "-json"}, toolchainVars...)...)
	if err != nil {
		return KeyBase{}, fmt.Errorf(envFailed, err, gate.Tail(procexec.Output(err)))
	}
	pol, err := json.Marshal([]any{e.ws.Policy.Thresholds, e.ws.Policy.Limits, e.ws.App})
	if err != nil {
		return KeyBase{}, err
	}
	mod := []string{}
	d := newDigests(e.ws.Src, "", golang.Closure{}, nil)
	for _, f := range moduleFiles {
		mod = append(mod, f, d.content(filepath.Join(e.ws.Src, f)))
	}
	return KeyBase{Engine: EngineVersion, Policy: hashOf(string(pol)), Toolchain: hashOf(res.Stdout), Module: hashOf(mod...), Flags: strings.Join(engineFlags, " "), Host: hostProfile()}, nil
}

func (e *engine) testLog(pkg string) string {
	return filepath.Join(e.ws.Report(testLogDir), hashOf(pkg)+testLogExt)
}

func (e *engine) lookupEnv(name string) (string, bool) {
	if k, v, _ := strings.Cut(e.cache, "="); k == name {
		return v, true
	}
	return os.LookupEnv(name)
}

func (e *engine) closure(ctx context.Context, pkgs golang.Packages) (*digests, error) {
	base, err := e.keyBase(ctx)
	if err != nil {
		return nil, err
	}
	built, err := golang.ListClosure(ctx, e.ws, []string{e.cache})
	if err != nil {
		return nil, err
	}
	d := newDigests(e.ws.Src, base.Digest(), built, map[string]string{})
	for _, p := range pkgs {
		data, rerr := os.ReadFile(e.testLog(p.ImportPath))
		if rerr != nil {
			d.volatile[p.ImportPath] = true
			continue
		}
		d.reads[p.ImportPath] = d.readsDigest(p.Dir, string(data), e.lookupEnv)
	}
	return d, nil
}

func (e *engine) prior() Verdicts {
	c := e.ws.MutationCache
	if c.Path == "" {
		return Verdicts{}
	}
	v, err := LoadVerdicts(c.Path, c.SHA256)
	if err != nil {
		e.ws.Log.Error("mutation verdict cache ignored", "error", err.Error())
		return Verdicts{}
	}
	e.ws.Log.Info(logCacheLoaded, "verdicts", len(v.Entries))
	return v
}

func (e *engine) assignKeys(d *digests, targets []target) error {
	if EngineVersion == "" {
		e.ws.Log.Warn(logUnversioned)
		return nil
	}
	prior := e.prior()
	if len(prior.Entries) > 0 && !slices.Contains(prior.Bases, d.base) {
		e.ws.Log.Warn(logStaleCache)
	}
	for i := range targets {
		key, err := d.key(targets[i])
		if err != nil {
			return err
		}
		targets[i].verdict = key
		if s, ok := prior.lookup(key); ok {
			targets[i].cached = s
		}
	}
	return nil
}

func (e *engine) saveVerdicts(base string, targets []target, rep Report) error {
	v := Verdicts{Version: verdictsVersion, Bases: []string{base}, Entries: map[string]Status{}}
	for i, t := range targets {
		if s := rep.Entries[i].Status; t.verdict != "" && cacheable(s) {
			v.Entries[t.verdict] = s
		}
	}
	_, err := v.Save(e.ws.Report(VerdictsFile))
	return err
}

func (j Job) status(ctx context.Context, worker int) Status {
	if j.Cached != "" {
		return j.Cached
	}
	if s := j.Run(ctx, worker); ctx.Err() == nil {
		return s
	}
	return StatusErrored
}

func (e *engine) incremental(ctx context.Context, pkgs golang.Packages) (Report, error) {
	d, err := e.closure(ctx, pkgs)
	if err != nil {
		return Report{}, err
	}
	targets, err := e.targets(pkgs)
	if err == nil {
		err = e.assignKeys(d, targets)
	}
	if err == nil {
		e.index, err = e.buildIndex(ctx, pkgs, uncached(d, targets))
	}
	if err == nil {
		err = e.guard.check()
	}
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = os.RemoveAll(e.dir) }()
	defer e.dropWorkerCaches()
	if e.workers, err = e.workerCaches(Workers()); err != nil {
		return Report{}, fmt.Errorf("cannot seed the worker build caches: %w", err)
	}
	rep, err := e.execute(ctx, targets)
	if len(rep.Entries) == len(targets) {
		err = errors.Join(err, e.saveVerdicts(d.base, targets, rep))
	}
	return rep, err
}

func uncached(d *digests, targets []target) map[string]bool {
	need := map[string]bool{}
	for _, t := range targets {
		if t.cached != "" {
			continue
		}
		need[t.pkg.ImportPath] = true
		mark(need, t.dependents)
		mark(need, d.linkers(t.pkg.ImportPath))
	}
	return need
}

func mark(set map[string]bool, paths []string) {
	for _, p := range paths {
		set[p] = true
	}
}

func hostProfile() string {
	return fmt.Sprintf("cpus=%d workers=%d memory=%d", procexec.CPUs(), Workers(), MemoryBudget())
}
