package python

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	JUnitReport       = "junit.xml"
	CoverageReport    = "coverage.json"
	coverageData      = ".coverage"
	coverageConfig    = "coverage.ini"
	pluginDir         = "plugin"
	pluginModule      = "deploy_hub_pytest"
	pluginFile        = pluginModule + Source
	hypothesisDir     = "hypothesis"
	ValueLineCoverage = "line_coverage"
	ValueBranchCover  = "branch_coverage"
	ValueStatements   = "statements"
	ValueBranches     = "branches"
	ValueFuzzTests    = "fuzz_tests"
	percentScale      = 100
	exitNoTests       = 5
	reasonLines       = "line coverage is below the policy threshold"
	reasonBranches    = "branch coverage is below the policy threshold"
	reasonFuzz        = "property-based fuzz tests failed"
	reasonNoFuzz      = "no fuzz targets: input handlers and parsers must have hypothesis property tests"
	uncoveredForm     = "%s: %d of %d lines uncovered"
	nodeSeparator     = "::"
	pluginPerm        = 0o600
	pluginDirPerm     = 0o700
)

const pluginSource = `import os

import coverage
import pytest


def pytest_configure(config):
    examples = os.environ.get("DEPLOY_HUB_FUZZ_EXAMPLES")
    if examples:
        from hypothesis import settings
        from hypothesis.database import DirectoryBasedExampleDatabase

        settings.register_profile(
            "deploy-hub",
            max_examples=int(examples),
            deadline=None,
            database=DirectoryBasedExampleDatabase(os.environ["HYPOTHESIS_STORAGE_DIRECTORY"]),
        )
        settings.load_profile("deploy-hub")


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_call(item):
    cov = coverage.Coverage.current()
    if cov is not None:
        cov.switch_context(item.nodeid)
    yield
    if cov is not None:
        cov.switch_context("")
`

func pluginPath(ws gate.Workspace) (string, error) {
	dir := ws.Report(pluginDir)
	if err := os.MkdirAll(dir, pluginDirPerm); err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, pluginFile), []byte(pluginSource), pluginPerm)
}

func testEnv(ws gate.Workspace, plugins, hypothesis string) []string {
	return []string{"PYTHONPATH=" + plugins, "PYTHONDONTWRITEBYTECODE=1", "COVERAGE_CORE=ctrace", "HYPOTHESIS_STORAGE_DIRECTORY=" + hypothesis,
		"COVERAGE_RCFILE=" + policyFile(ws, coverageConfig)}
}

func pytestArgs(extra ...string) []string {
	return append([]string{flagModule, "pytest", flagPlugin, pluginModule, flagPlugin, "no:cacheprovider", "-o", "addopts=", "-q"}, extra...)
}

func Tests(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	plugins, err := pluginPath(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	e := projectEnv(ws)
	envs := append(testEnv(ws, plugins, ws.Report(hypothesisDir)), "COVERAGE_FILE="+ws.Report(coverageData))
	args := append([]string{flagModule, "coverage", cmdRun, "--branch", "--source=."}, pytestArgs("-o", "junit_family=xunit1", "--junitxml="+ws.Report(JUnitReport))...)
	_, runErr := e.python(ctx, envs, args...)
	cases, lerr := junit.Load(ws.Report(JUnitReport))
	if lerr != nil {
		return gate.Fail("pytest produced no report: " + gate.Tail(errText(runErr, lerr)))
	}
	if _, cerr := e.python(ctx, envs, flagModule, "coverage", "json", "-q", "--show-contexts", "-o", ws.Report(CoverageReport)); cerr != nil {
		return gate.Fail(cerr.Error())
	}
	return junit.Judge(cases, nodeID)
}

func errText(errs ...error) string {
	return errors.Join(errs...).Error()
}

func nodeID(c junit.Case) string {
	if c.File == "" {
		return c.Classname + "." + c.Name
	}
	module := strings.ReplaceAll(strings.TrimSuffix(c.File, Source), "/", ".")
	class := strings.TrimPrefix(strings.TrimPrefix(c.Classname, module), ".")
	if class == "" {
		return c.File + nodeSeparator + c.Name
	}
	return c.File + nodeSeparator + strings.ReplaceAll(class, ".", nodeSeparator) + nodeSeparator + c.Name
}

type coverageFile struct {
	Executed []int               `json:"executed_lines"`
	Missing  []int               `json:"missing_lines"`
	Contexts map[string][]string `json:"contexts"`
}

type coverageJSON struct {
	Totals struct {
		CoveredLines    int `json:"covered_lines"`
		Statements      int `json:"num_statements"`
		CoveredBranches int `json:"covered_branches"`
		Branches        int `json:"num_branches"`
	} `json:"totals"`
	Files map[string]coverageFile `json:"files"`
}

func loadCoverage(ws gate.Workspace) (coverageJSON, error) {
	var c coverageJSON
	data, err := os.ReadFile(filepath.Clean(ws.Report(CoverageReport)))
	if err != nil {
		return c, fmt.Errorf("coverage report missing: run the tests check first: %w", err)
	}
	return c, json.Unmarshal(data, &c)
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return percentScale
	}
	return float64(part) * percentScale / float64(whole)
}

func LineCoverage(_ context.Context, ws gate.Workspace) gate.Outcome {
	c, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	pct := ratio(c.Totals.CoveredLines, c.Totals.Statements)
	var findings []string
	for name, f := range c.Files {
		if len(f.Missing) > 0 {
			findings = append(findings, fmt.Sprintf(uncoveredForm, name, len(f.Missing), len(f.Missing)+len(f.Executed)))
		}
	}
	values := map[string]float64{ValueLineCoverage: pct, ValueStatements: float64(c.Totals.Statements)}
	return gate.Judge(pct >= ws.Policy.Thresholds.LineCoverage, reasonLines, values, sortedStrings(findings))
}

func Branches(_ context.Context, ws gate.Workspace) gate.Outcome {
	c, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	pct := ratio(c.Totals.CoveredBranches, c.Totals.Branches)
	values := map[string]float64{ValueBranchCover: pct, ValueBranches: float64(c.Totals.Branches)}
	return gate.Judge(pct >= ws.Policy.Thresholds.BranchCoverage, reasonBranches, values, nil)
}

func Fuzz(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	plugins, err := pluginPath(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	envs := append(testEnv(ws, plugins, ws.Report(hypothesisDir)), fmt.Sprintf("DEPLOY_HUB_FUZZ_EXAMPLES=%d", ws.Policy.Limits.FuzzExamples))
	report := ws.Report("fuzz-" + JUnitReport)
	_, runErr := projectEnv(ws).python(ctx, envs, pytestArgs("-m", "hypothesis", "-o", "junit_family=xunit1", "--junitxml="+report)...)
	if exitCode(runErr) == exitNoTests {
		return gate.Fail(reasonNoFuzz)
	}
	cases, lerr := junit.Load(report)
	if lerr != nil {
		return gate.Fail("pytest produced no fuzz report: " + gate.Tail(errText(runErr, lerr)))
	}
	out := junit.Judge(cases, nodeID)
	out.Values = map[string]float64{ValueFuzzTests: float64(len(cases))}
	if out.Status == gate.StatusFail {
		out.Reason = reasonFuzz
	}
	return out
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

func Passed(_ context.Context, ws gate.Workspace) (map[string]bool, error) {
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return nil, err
	}
	return junit.Passed(cases, nodeID), nil
}

func isTest(rel string) bool {
	base := path.Base(rel)
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test"+Source) || base == "conftest"+Source ||
		strings.HasPrefix(rel, "tests/") || strings.Contains(rel, "/tests/")
}

func sourceFiles(ctx context.Context, ws gate.Workspace) ([]string, error) {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	var out []string
	for _, f := range files {
		if path.Ext(f) == Source || path.Ext(f) == Stubs {
			out = append(out, f)
		}
	}
	return out, err
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
