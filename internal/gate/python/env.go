package python

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	UVBinary     = "uv"
	RuffBinary   = "ruff"
	Stack        = "python"
	Source       = ".py"
	Stubs        = ".pyi"
	ProjectFile  = "pyproject.toml"
	lockFile     = "uv.lock"
	venvDir      = "venv"
	toolsDir     = "pytools"
	cacheDir     = "uv-cache"
	readyStamp   = "venv.ready"
	policySub    = "python"
	toolsReqs    = "tools-lock.txt"
	testReqs     = "test-lock.txt"
	interpreter  = "python"
	stampPerm    = 0o600
	flagQuiet    = "--quiet"
	flagModule   = "-m"
	flagPlugin   = "-p"
	cmdRun       = "run"
	flagPython   = "--python"
	flagRequire  = "--require-hashes"
	flagReqFile  = "-r"
	testImports  = "import pytest, coverage, hypothesis"
	envCache     = "UV_CACHE_DIR="
	envProject   = "UV_PROJECT_ENVIRONMENT="
	envNoProg    = "UV_NO_PROGRESS=1"
	reasonNoLock = "pyproject.toml without uv.lock: dependency versions are not locked"
)

var (
	ErrNoLock        = errors.New(reasonNoLock)
	requirementsFile = regexp.MustCompile(`^requirements.*\.txt$`)
)

type layout int

const (
	layoutLoose layout = iota
	layoutUV
	layoutUnlocked
	layoutRequirements
)

func detect(dir string) (layout, []string, error) {
	switch {
	case exists(filepath.Join(dir, lockFile)):
		return layoutUV, nil, nil
	case exists(filepath.Join(dir, ProjectFile)):
		return layoutUnlocked, nil, ErrNoLock
	}
	reqs, err := requirementFiles(dir)
	if len(reqs) > 0 {
		return layoutRequirements, reqs, err
	}
	return layoutLoose, nil, err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func requirementFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && requirementsFile.MatchString(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, err
}

func venvPython(venv string) string {
	return filepath.Join(venv, "bin", interpreter)
}

type env struct {
	ws    gate.Workspace
	src   string
	venv  string
	cache string
}

func newEnv(ws gate.Workspace, src, venv string) env {
	return env{ws: ws, src: src, venv: venv, cache: ws.Report(cacheDir)}
}

func (e env) uv(ctx context.Context, extra []string, args ...string) (procexec.Result, error) {
	ws := e.ws
	ws.Src = e.src
	res, err := ws.RunEnv(ctx, append([]string{envCache + e.cache, envNoProg}, extra...), UVBinary, args...)
	if err != nil {
		return res, fmt.Errorf("uv %s: %w: %s", args[0], err, gate.Tail(procexec.Output(err)))
	}
	return res, nil
}

func (e env) python(ctx context.Context, extra []string, args ...string) (procexec.Result, error) {
	return e.tool(ctx, extra, interpreter, args...)
}

func (e env) tool(ctx context.Context, extra []string, name string, args ...string) (procexec.Result, error) {
	return e.uv(ctx, extra, append([]string{cmdRun, flagQuiet, "--no-project", flagPython, venvPython(e.venv), "--", name}, args...)...)
}

func (e env) install(ctx context.Context, requirements string) error {
	_, err := e.uv(ctx, nil, "pip", "install", flagQuiet, flagPython, venvPython(e.venv), flagRequire, flagReqFile, requirements)
	return err
}

func (e env) create(ctx context.Context) error {
	kind, reqs, err := detect(e.src)
	if err != nil {
		return err
	}
	if kind == layoutUV {
		_, err = e.uv(ctx, []string{envProject + e.venv}, "sync", flagQuiet, "--locked", "--all-groups", "--all-extras")
		return err
	}
	if _, err = e.uv(ctx, nil, "venv", flagQuiet, e.venv); err != nil {
		return err
	}
	for _, r := range reqs {
		if err = e.install(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (e env) withTestTools(ctx context.Context) error {
	if _, err := e.python(ctx, nil, "-c", testImports); err == nil {
		return nil
	}
	return e.install(ctx, filepath.Join(e.ws.PolicyDir, policySub, testReqs))
}

var (
	prepared   = pack.Prepared(prepare)
	Lint       = prepared(runLint)
	Complexity = prepared(runComplexity)
	DeadCode   = prepared(runDeadCode)
	Tests      = prepared(runTests)
	Fuzz       = prepared(runFuzz)
)

func prepare(ctx context.Context, ws gate.Workspace) error {
	if exists(ws.Report(readyStamp)) {
		return nil
	}
	project := newEnv(ws, ws.Src, ws.Report(venvDir))
	tools := newEnv(ws, ws.Src, ws.Report(toolsDir))
	steps := []func(context.Context) error{
		project.create,
		project.withTestTools,
		func(ctx context.Context) error {
			_, err := tools.uv(ctx, nil, "venv", flagQuiet, tools.venv)
			return err
		},
		func(ctx context.Context) error {
			return tools.install(ctx, filepath.Join(ws.PolicyDir, policySub, toolsReqs))
		},
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return os.WriteFile(ws.Report(readyStamp), []byte(Stack), stampPerm)
}

func projectEnv(ws gate.Workspace) env {
	return newEnv(ws, ws.Src, ws.Report(venvDir))
}

func toolsEnv(ws gate.Workspace) env {
	return newEnv(ws, ws.Src, ws.Report(toolsDir))
}
