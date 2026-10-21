package sqlfiles

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Stack         = "sql"
	Source        = ".sql"
	UVBinary      = "uv"
	sqlfluff      = "sqlfluff"
	parseError    = "PRS"
	policySub     = "sql"
	toolsLock     = "tools-lock.txt"
	venvDir       = "sqltools"
	cacheDir      = "uv-cache"
	readyStamp    = "sqltools.ready"
	findingForm   = "%s:%d %s %s"
	envCache      = "UV_CACHE_DIR="
	envNoProgress = "UV_NO_PROGRESS=1"
	flagQuiet     = "--quiet"
	cmdRun        = "run"
	flagPython    = "--python"
	reasonLint    = "SQL files do not parse in any supported dialect"
	reasonNoCode  = "SQL scripts are data for the program that runs them; its tests cover them"
	reasonNoDeps  = "SQL scripts have no package dependencies"
)

var (
	Sources  = []string{Source}
	dialects = []string{"sqlite", "postgres", "mysql", "tsql", "ansi"}
	NoCode   = gate.NotApplicable(reasonNoCode)
	NoDeps   = gate.NotApplicable(reasonNoDeps)
)

func Passed(context.Context, gate.Workspace) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func uv(ctx context.Context, ws gate.Workspace, args ...string) (procexec.Result, error) {
	res, err := ws.RunEnv(ctx, []string{envCache + ws.Report(cacheDir), envNoProgress}, UVBinary, args...)
	if err != nil {
		return res, fmt.Errorf("uv %s: %w: %s", args[0], err, gate.Tail(procexec.Output(err)))
	}
	return res, nil
}

func python(ws gate.Workspace) string {
	return filepath.Join(ws.Report(venvDir), "bin", "python")
}

func install(ctx context.Context, ws gate.Workspace) error {
	if _, err := uv(ctx, ws, "venv", flagQuiet, ws.Report(venvDir)); err != nil {
		return err
	}
	_, err := uv(ctx, ws, "pip", "install", flagQuiet, flagPython, python(ws), "--require-hashes", "-r",
		filepath.Join(ws.PolicyDir, policySub, toolsLock))
	return err
}

type report struct {
	Path       string `json:"filepath"`
	Violations []struct {
		Code        string `json:"code"`
		Line        int    `json:"start_line_no"`
		Description string `json:"description"`
	} `json:"violations"`
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Select(ctx, ws, func(f string) bool { return path.Ext(f) == Source && !source.Fixture(f) })
	if err == nil && len(files) > 0 {
		err = pack.Once(ctx, ws, readyStamp, install)
	}
	var findings []string
	if err == nil {
		findings, err = unparsed(ctx, ws, files)
	}
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonLint, findings)
}

func unparsed(ctx context.Context, ws gate.Workspace, files []string) ([]string, error) {
	var first map[string][]string
	for _, dialect := range dialects {
		if len(files) == 0 {
			return nil, nil
		}
		found, err := parse(ctx, ws, dialect, files)
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = found
		}
		files = slices.DeleteFunc(files, func(f string) bool { return len(found[f]) == 0 })
	}
	var findings []string
	for _, f := range files {
		findings = append(findings, first[f]...)
	}
	return findings, nil
}

func parse(ctx context.Context, ws gate.Workspace, dialect string, files []string) (map[string][]string, error) {
	args := append([]string{cmdRun, flagQuiet, "--no-project", flagPython, python(ws), "--", sqlfluff, "lint", "--dialect", dialect,
		"--rules", "LT01", "--format", "json", "--nofail", "--disable-progress-bar"}, files...)
	res, err := uv(ctx, ws, args...)
	if err != nil {
		return nil, err
	}
	var reports []report
	if perr := json.Unmarshal([]byte(res.Stdout), &reports); perr != nil {
		return nil, fmt.Errorf("%s: %w: %s", sqlfluff, perr, gate.Tail(res.Stdout))
	}
	out := map[string][]string{}
	for _, r := range reports {
		rel := ws.Rel(r.Path)
		for _, v := range r.Violations {
			if v.Code == parseError {
				out[rel] = append(out[rel], fmt.Sprintf(findingForm, rel, v.Line, v.Code, v.Description))
			}
		}
	}
	return out, nil
}
