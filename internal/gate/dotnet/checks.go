package dotnet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	approvedPrefix    = "nuget:"
	diagnosticsReport = "diagnostics.json"
	complexityRule    = "S3776"
	projectType       = "Project"
	reasonIntegrity   = "dependencies are not locked"
	reasonLint        = "compiler, analyzers or formatting reported findings"
	reasonComplexity  = "functions exceed the cognitive complexity limit"
	reasonDeadCode    = "unused code"
	noLockForm        = "%s: no %s next to it (set RestorePackagesWithLockFile)"
	diagnosticForm    = "%s:%s %s %s"
	suppressionForm   = "%s:%d analyzer suppression: %s"
	formatForm        = "%s: not formatted"
	reportPerm        = 0o600
)

var (
	diagnosticLine  = regexp.MustCompile(`^(.+?)\((\d+),\d+\): (?:error|warning) ([A-Z]+\d+): (.*?)(?: \[[^\]]*\])?$`)
	formatLine      = regexp.MustCompile(`^(.+?)\(\d+,\d+\): (?:error|warning) WHITESPACE: `)
	suppressions    = regexp.MustCompile(`#pragma\s+warning\s+disable|SuppressMessage|ExcludeFromCodeCoverage`)
	deadCodeRules   = []string{"S1144", "S1172", "S4487", "IDE0051", "IDE0052", "IDE0060", "CS0168", "CS0169", "CS0219"}
	nonLintRules    = append([]string{complexityRule}, deadCodeRules...)
	suppressedFiles = Sources
)

func Integrity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Select(ctx, ws, func(string) bool { return true })
	if err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	for _, f := range files {
		if isProject(f) && !slices.Contains(files, path.Join(path.Dir(f), lockFile)) {
			findings = append(findings, fmt.Sprintf(noLockForm, f, lockFile))
		}
	}
	return pack.Locked(ctx, ws, reasonIntegrity, findings, prepare)
}

type nugetLock struct {
	Dependencies map[string]map[string]struct {
		Type string `json:"type"`
	} `json:"dependencies"`
}

func Dependencies(ctx context.Context, ws gate.Workspace) gate.Outcome {
	locks, err := source.Select(ctx, ws, func(f string) bool { return path.Base(f) == lockFile })
	if err != nil {
		return gate.Fail(err.Error())
	}
	names, err := lockedNames(ws.Src, locks)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return pack.Approval(ws, approvedPrefix, names, strings.ToLower)
}

func lockedNames(dir string, locks []string) ([]string, error) {
	var names []string
	for _, l := range locks {
		lock, err := readNugetLock(filepath.Join(dir, filepath.FromSlash(l)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l, err)
		}
		for _, framework := range lock.Dependencies {
			names = appendPackages(names, framework)
		}
	}
	slices.Sort(names)
	return names, nil
}

func readNugetLock(file string) (nugetLock, error) {
	var lock nugetLock
	data, err := os.ReadFile(filepath.Clean(file))
	if err == nil {
		err = json.Unmarshal(data, &lock)
	}
	return lock, err
}

func appendPackages(names []string, framework map[string]struct {
	Type string `json:"type"`
}) []string {
	for name, dep := range framework {
		if dep.Type != projectType && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

type diagnostic struct {
	File    string `json:"file"`
	Line    string `json:"line"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

func (d diagnostic) String() string {
	return fmt.Sprintf(diagnosticForm, d.File, d.Line, d.Rule, d.Message)
}

func analyze(ctx context.Context, ws gate.Workspace) ([]diagnostic, error) {
	if data, err := os.ReadFile(ws.Report(diagnosticsReport)); err == nil {
		var cached []diagnostic
		return cached, json.Unmarshal(data, &cached)
	}
	if err := prepare(ctx, ws); err != nil {
		return nil, err
	}
	args := append([]string{cmdBuild, flagNoRestore, flagVerbosity, verbosityQuiet, "-clp:NoSummary"}, analysisProps(ws)...)
	res, err := run(ctx, ws, stage(ws), nil, args...)
	found := parseDiagnostics(ws, res.Stdout+procexec.Output(err))
	if err != nil && len(found) == 0 {
		return nil, err
	}
	data, merr := json.Marshal(found)
	if merr == nil {
		merr = os.WriteFile(ws.Report(diagnosticsReport), data, reportPerm)
	}
	return found, merr
}

func parseDiagnostics(ws gate.Workspace, text string) []diagnostic {
	var out []diagnostic
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		m := diagnosticLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		d := diagnostic{File: rel(ws, m[1]), Line: m[2], Rule: m[3], Message: strings.TrimSpace(m[4])}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func selected(ds []diagnostic, keep func(string) bool) []string {
	var out []string
	for _, d := range ds {
		if keep(d.Rule) {
			out = append(out, d.String())
		}
	}
	return out
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	ds, err := analyze(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	findings := selected(ds, func(rule string) bool { return !slices.Contains(nonLintRules, rule) })
	format, err := formatting(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	suppressed, err := suppressionFindings(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonLint, slices.Concat(findings, format, suppressed))
}

func formatting(ctx context.Context, ws gate.Workspace) ([]string, error) {
	res, err := run(ctx, ws, stage(ws), nil, "format", "whitespace", "--verify-no-changes", flagVerbosity, verbosityQuiet)
	var out []string
	sc := bufio.NewScanner(strings.NewReader(res.Stdout + procexec.Output(err)))
	for sc.Scan() {
		if m := formatLine.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil && !slices.Contains(out, fmt.Sprintf(formatForm, rel(ws, m[1]))) {
			out = append(out, fmt.Sprintf(formatForm, rel(ws, m[1])))
		}
	}
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func suppressionFindings(ctx context.Context, ws gate.Workspace) ([]string, error) {
	files, err := source.Select(ctx, ws, func(f string) bool { return slices.Contains(suppressedFiles, path.Ext(f)) })
	var out []string
	for _, f := range files {
		data, rerr := os.ReadFile(filepath.Clean(filepath.Join(ws.Src, filepath.FromSlash(f))))
		if rerr != nil {
			return nil, rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if m := suppressions.FindString(line); m != "" {
				out = append(out, fmt.Sprintf(suppressionForm, f, i+1, m))
			}
		}
	}
	return out, err
}

var (
	Complexity = rules(reasonComplexity, func(rule string) bool { return rule == complexityRule })
	DeadCode   = rules(reasonDeadCode, func(rule string) bool { return slices.Contains(deadCodeRules, rule) })
)

func rules(reason string, keep func(string) bool) func(context.Context, gate.Workspace) gate.Outcome {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		ds, err := analyze(ctx, ws)
		if err != nil {
			return gate.Fail(err.Error())
		}
		return gate.Findings(reason, selected(ds, keep))
	}
}
