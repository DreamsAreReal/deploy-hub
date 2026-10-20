package python

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ValueDependencies = "dependencies"
	ValueFunctions    = "functions"
	ValueMaxCognitive = "max_cognitive_complexity"
	approvedPrefix    = "pypi:"
	ruffConfig        = "ruff.toml"
	mypyConfig        = "mypy.ini"
	vultureConfig     = "vulture.toml"
	complexityReport  = "complexity.json"
	mypyCache         = "mypy-cache"
	complexityCache   = "complexipy-cache"
	reasonIntegrity   = "dependencies are not pinned and hashed"
	reasonDeps        = "dependencies are not approved"
	reasonLint        = "linters and the strict type checker reported findings"
	reasonComplexity  = "functions exceed the cognitive complexity limit"
	reasonDeadCode    = "unused code"
	unpinnedForm      = "%s: %s is not pinned to one version with hashes"
	unapprovedForm    = "%s is not in the approved dependency list"
	ruffForm          = "%s:%d %s %s"
	mypyForm          = "%s:%d %s [%s]"
	complexityForm    = "%s: %s has cognitive complexity %d"
	srcLayout         = "src"
	formatJSON        = "json"
	complexipyTool    = "complexipy"
)

var (
	requirementLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(\[[^\]]*\])?\s*([=<>!~]=?|$)`)
	nameSeparators  = regexp.MustCompile(`[-_.]+`)
)

func policyFile(ws gate.Workspace, name string) string {
	return filepath.Join(ws.PolicyDir, policySub, name)
}

func Integrity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	kind, reqs, err := detect(ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	if kind == layoutUV {
		if _, lerr := projectEnv(ws).uv(ctx, nil, "lock", "--check", flagQuiet); lerr != nil {
			findings = append(findings, lerr.Error())
		}
	}
	for _, r := range reqs {
		findings = append(findings, unhashed(r)...)
	}
	if len(findings) == 0 {
		if perr := prepare(ctx, ws); perr != nil {
			findings = append(findings, perr.Error())
		}
	}
	return gate.Findings(reasonIntegrity, findings)
}

func unhashed(path string) []string {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return []string{err.Error()}
	}
	var findings []string
	for _, req := range logicalLines(string(data)) {
		m := requirementLine.FindStringSubmatch(req)
		if m != nil && (m[3] != "==" || !strings.Contains(req, "--hash=")) {
			findings = append(findings, fmt.Sprintf(unpinnedForm, filepath.Base(path), m[1]))
		}
	}
	return findings
}

func logicalLines(text string) []string {
	var out []string
	var cur strings.Builder
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		cur.WriteString(strings.TrimSuffix(line, "\\") + " ")
		if !strings.HasSuffix(line, "\\") {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	return out
}

func Dependencies(ctx context.Context, ws gate.Workspace) gate.Outcome {
	names, err := dependencyNames(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	approved := map[string]bool{}
	for _, d := range ws.Policy.Dependencies {
		if name, ok := strings.CutPrefix(d, approvedPrefix); ok {
			approved[normalize(name)] = true
		}
	}
	var findings []string
	for _, n := range names {
		if !approved[n] {
			findings = append(findings, fmt.Sprintf(unapprovedForm, approvedPrefix+n))
		}
	}
	values := map[string]float64{ValueDependencies: float64(len(names))}
	return gate.Judge(len(findings) == 0, reasonDeps, values, findings)
}

func dependencyNames(ctx context.Context, ws gate.Workspace) ([]string, error) {
	kind, reqs, err := detect(ws.Src)
	if err != nil {
		return nil, err
	}
	text := ""
	if kind == layoutUV {
		res, uerr := projectEnv(ws).uv(ctx, nil, "export", flagQuiet, "--locked", "--all-groups", "--all-extras", "--format", "requirements-txt",
			"--no-hashes", "--no-emit-project", "--no-header", "--no-annotate")
		text, err = res.Stdout, uerr
	}
	for _, r := range reqs {
		data, rerr := os.ReadFile(filepath.Clean(r))
		text, err = text+"\n"+string(data), rerr
	}
	return requirementNames(text), err
}

func requirementNames(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range logicalLines(text) {
		m := requirementLine.FindStringSubmatch(line)
		if m == nil || seen[normalize(m[1])] {
			continue
		}
		seen[normalize(m[1])] = true
		out = append(out, normalize(m[1]))
	}
	return out
}

func normalize(name string) string {
	return nameSeparators.ReplaceAllString(strings.ToLower(name), "-")
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	for _, run := range []func(context.Context, gate.Workspace) ([]string, error){ruffCheck, ruffFormat, mypy} {
		found, err := run(ctx, ws)
		if err != nil {
			return gate.Fail(err.Error())
		}
		findings = append(findings, found...)
	}
	return gate.Findings(reasonLint, findings)
}

func ruffCheck(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return ruff(ctx, ws, "check")
}

func ruffFormat(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return ruff(ctx, ws, "format", "--check")
}

func ruff(ctx context.Context, ws gate.Workspace, sub ...string) ([]string, error) {
	args := slices.Concat(sub, []string{"--config", policyFile(ws, ruffConfig), "--no-cache", "--output-format", formatJSON, ws.Src})
	res, err := ws.Run(ctx, RuffBinary, args...)
	var issues []struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Filename string `json:"filename"`
		Location struct {
			Row int `json:"row"`
		} `json:"location"`
	}
	if perr := json.Unmarshal([]byte(res.Stdout), &issues); perr != nil {
		return nil, toolError("ruff "+sub[0], err, perr)
	}
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, fmt.Sprintf(ruffForm, ws.Rel(i.Filename), i.Location.Row, i.Code, i.Message))
	}
	return out, nil
}

func toolError(tool string, runErr, parseErr error) error {
	return fmt.Errorf("%s: %w: %s", tool, parseErr, gate.Tail(procexec.Output(runErr)))
}

func mypy(ctx context.Context, ws gate.Workspace) ([]string, error) {
	files, err := sourceFiles(ctx, ws)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	args := append([]string{flagModule, "mypy", "--config-file", policyFile(ws, mypyConfig), "--python-executable", venvPython(ws.Report(venvDir)),
		"--cache-dir", ws.Report(mypyCache), "--explicit-package-bases", "--no-error-summary", "-O", "json"}, files...)
	mypyPath := "MYPYPATH=" + filepath.Join(ws.Src, srcLayout) + string(os.PathListSeparator) + ws.Src
	res, runErr := toolsEnv(ws).python(ctx, []string{mypyPath}, args...)
	out := parseMypy(res.Stdout+procexec.Output(runErr), ws.Rel)
	if runErr != nil && len(out) == 0 {
		return nil, runErr
	}
	return out, nil
}

func parseMypy(text string, rel func(string) string) []string {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		var issue struct {
			File    string `json:"file"`
			Line    int    `json:"line"`
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if json.Unmarshal(sc.Bytes(), &issue) != nil || issue.File == "" {
			continue
		}
		if line := fmt.Sprintf(mypyForm, rel(issue.File), issue.Line, issue.Message, issue.Code); !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

func Complexity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	report := ws.Report(complexityReport)
	_, err := toolsEnv(ws).tool(ctx, nil, complexipyTool, ws.Src, "--max-complexity-allowed", fmt.Sprint(ws.Policy.Thresholds.MaxCognitive),
		"--quiet", "--no-ignore", "--output-format", formatJSON, "--output", report, "--cache-dir", ws.Report(complexityCache))
	data, rerr := os.ReadFile(filepath.Clean(report))
	var functions []struct {
		Complexity int    `json:"complexity"`
		Function   string `json:"function_name"`
		Path       string `json:"path"`
	}
	if perr := json.Unmarshal(data, &functions); rerr != nil || perr != nil {
		return gate.Fail(fmt.Sprintf("complexipy: %v %v %v", err, rerr, perr))
	}
	var findings []string
	worst := 0
	for _, f := range functions {
		worst = max(worst, f.Complexity)
		if f.Complexity > ws.Policy.Thresholds.MaxCognitive {
			findings = append(findings, fmt.Sprintf(complexityForm, ws.Rel(f.Path), f.Function, f.Complexity))
		}
	}
	values := map[string]float64{ValueFunctions: float64(len(functions)), ValueMaxCognitive: float64(worst)}
	return gate.Judge(len(findings) == 0, reasonComplexity, values, findings)
}

func DeadCode(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	res, err := toolsEnv(ws).python(ctx, nil, flagModule, "vulture", "--config", policyFile(ws, vultureConfig), ws.Src)
	text := res.Stdout + procexec.Output(err)
	var findings []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "% confidence)") {
			findings = append(findings, ws.Rel(strings.TrimSpace(line)))
		}
	}
	if err != nil && len(findings) == 0 {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonDeadCode, findings)
}
