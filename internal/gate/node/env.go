package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	NodeBinary    = "node"
	NPMBinary     = "npm"
	Stack         = "node"
	Manifest      = "package.json"
	Lockfile      = "package-lock.json"
	stageDir      = "src"
	toolsDir      = "node-tools"
	cacheDir      = "npm-cache"
	readyStamp    = "node.ready"
	toolsStamp    = "node-tools.ready"
	policySub     = "node"
	toolsManifest = "tools.json"
	toolsLock     = "tools-lock.json"
	modulesDir    = "node_modules"
	stampPerm     = 0o600
	toolsDirPerm  = 0o700
	reasonNoLock  = "package.json without package-lock.json: dependency versions are not locked"
	envCache      = "npm_config_cache="
	flagNoAudit   = "--no-audit"
	flagNoFund    = "--no-fund"
	extJS         = ".js"
	extMJS        = ".mjs"
	extCJS        = ".cjs"
	extJSX        = ".jsx"
	extTS         = ".ts"
	extMTS        = ".mts"
	extCTS        = ".cts"
	extTSX        = ".tsx"
)

var (
	ErrNoLock = errors.New(reasonNoLock)
	scripts   = []string{extJS, extMJS, extCJS, extJSX, extTS, extMTS, extCTS, extTSX}
	Sources   = append(slices.Clone(scripts), ".vue", ".svelte")
)

type runner string

const (
	runnerVitest runner = "vitest"
	runnerJest   runner = "jest"
	runnerMocha  runner = "mocha"
	runnerNode   runner = "node:test"
)

type manifest struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (m manifest) uses(pkg string) bool {
	_, dep := m.Dependencies[pkg]
	_, dev := m.DevDependencies[pkg]
	return dep || dev
}

func (m manifest) runner() runner {
	for _, r := range []runner{runnerVitest, runnerJest, runnerMocha} {
		if m.uses(string(r)) {
			return r
		}
	}
	return runnerNode
}

func readManifest(dir string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, Manifest)))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	return m, err
}

func stage(ws gate.Workspace) string {
	return ws.Report(stageDir)
}

func tools(ws gate.Workspace) string {
	return ws.Report(toolsDir)
}

func toolScript(ws gate.Workspace, parts ...string) string {
	return filepath.Join(append([]string{tools(ws), modulesDir}, parts...)...)
}

func run(ctx context.Context, ws gate.Workspace, dir string, env []string, name string, args ...string) (procexec.Result, error) {
	ws.Src = dir
	res, err := ws.RunEnv(ctx, append([]string{envCache + ws.Report(cacheDir)}, env...), name, args...)
	if err != nil {
		return res, fmt.Errorf("%s %s: %w: %s", name, firstArg(args), err, gate.Tail(procexec.Output(err)))
	}
	return res, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return filepath.Base(args[0])
}

func node(ctx context.Context, ws gate.Workspace, env []string, args ...string) (procexec.Result, error) {
	return run(ctx, ws, stage(ws), env, NodeBinary, args...)
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
	return pack.Once(ctx, ws, readyStamp, requireLock, stageProject, installTools)
}

func requireLock(_ context.Context, ws gate.Workspace) error {
	if _, err := os.Stat(filepath.Join(ws.Src, Lockfile)); err != nil {
		return ErrNoLock
	}
	return nil
}

func stageProject(ctx context.Context, ws gate.Workspace) error {
	if err := source.CopyTracked(ctx, ws, stage(ws)); err != nil {
		return err
	}
	_, err := run(ctx, ws, stage(ws), nil, NPMBinary, "ci", flagNoAudit, flagNoFund)
	return err
}

func installTools(ctx context.Context, ws gate.Workspace) error {
	dir := tools(ws)
	if err := os.MkdirAll(dir, toolsDirPerm); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for from, to := range map[string]string{toolsManifest: Manifest, toolsLock: Lockfile} {
		data, rerr := os.ReadFile(filepath.Clean(filepath.Join(ws.PolicyDir, policySub, from)))
		if rerr == nil {
			rerr = root.WriteFile(to, data, stampPerm)
		}
		if rerr != nil {
			return rerr
		}
	}
	if _, err := run(ctx, ws, dir, nil, NPMBinary, "ci", "--ignore-scripts", flagNoAudit, flagNoFund); err != nil {
		return err
	}
	return writeScripts(dir, ws.Policy.Thresholds.MaxCognitive)
}

func Tools(ctx context.Context, ws gate.Workspace) (string, error) {
	return tools(ws), pack.Once(ctx, ws, toolsStamp, installTools)
}

func ToolScript(ws gate.Workspace, parts ...string) string {
	return toolScript(ws, parts...)
}

func installedVersion(dir, pkg string) (string, error) {
	var m struct {
		Version string `json:"version"`
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, modulesDir, pkg, Manifest)))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	return m.Version, err
}

func rel(ws gate.Workspace, path string) string {
	return gate.RelTo(stage(ws), path)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
