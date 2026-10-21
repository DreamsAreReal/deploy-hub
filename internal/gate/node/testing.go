package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	JUnitReport     = "junit.xml"
	JestReport      = "jest.json"
	coverageDir     = "coverage"
	istanbulFile    = "coverage-final.json"
	lcovFile        = "lcov.info"
	v8Dir           = "v8"
	fuzzPrefix      = "fuzz-"
	ValueFuzzFiles  = "fuzz_files"
	fastCheck       = "fast-check"
	idSeparator     = " > "
	reasonFuzz      = "property-based fuzz tests failed"
	reasonNoFuzz    = "no fuzz targets: input handlers and parsers must have fast-check property tests"
	reasonNoReport  = "the test runner produced no report"
	envFuzzRuns     = "DEPLOY_HUB_FUZZ_RUNS="
	envNodeOptions  = "NODE_OPTIONS=--import="
	flagJUnit       = "--reporter=junit"
	flagCI          = "--ci"
	flagJSON        = "--json"
	flagBail        = "--bail"
	flagReporter    = "--reporter"
	flagTest        = "--test"
	cmdRun          = "run"
	reporterXUnit   = "xunit"
	lcovEnd         = "end_of_record"
	flagReporterOpt = "--reporter-option"
	flagTestJUnit   = "--test-reporter=junit"
)

type adapter struct {
	test   func(ws gate.Workspace) []string
	mutant func(ws gate.Workspace) []string
	fuzz   func(ws gate.Workspace, files []string) ([]string, []string)
}

func local(ws gate.Workspace, parts ...string) string {
	return filepath.Join(append([]string{stage(ws), modulesDir}, parts...)...)
}

var adapters = map[runner]adapter{
	runnerVitest: {
		test: func(ws gate.Workspace) []string {
			return []string{local(ws, "vitest", "vitest.mjs"), cmdRun, flagJUnit, "--outputFile=" + ws.Report(JUnitReport), "--coverage.enabled",
				"--coverage.provider=v8", "--coverage.reporter=json", "--coverage.reportsDirectory=" + ws.Report(coverageDir)}
		},
		mutant: func(ws gate.Workspace) []string {
			return []string{local(ws, "vitest", "vitest.mjs"), cmdRun, "--bail=1", "--reporter=dot"}
		},
		fuzz: func(ws gate.Workspace, files []string) ([]string, []string) {
			args := append([]string{local(ws, "vitest", "vitest.mjs"), cmdRun, flagJUnit, "--outputFile=" + ws.Report(fuzzPrefix+JUnitReport)}, files...)
			return args, []string{envNodeOptions + filepath.Join(tools(ws), fuzzSetupFile)}
		},
	},
	runnerJest: {
		test: func(ws gate.Workspace) []string {
			return []string{local(ws, "jest", "bin", "jest.js"), flagCI, "--coverage", "--coverageProvider=v8", "--coverageReporters=json",
				"--coverageDirectory=" + ws.Report(coverageDir), "--collectCoverageFrom=**/*.{js,mjs,cjs,jsx,ts,mts,cts,tsx}",
				"--collectCoverageFrom=!**/node_modules/**", flagJSON, "--outputFile=" + ws.Report(JestReport)}
		},
		mutant: func(ws gate.Workspace) []string {
			return []string{local(ws, "jest", "bin", "jest.js"), flagCI, flagBail}
		},
		fuzz: func(ws gate.Workspace, files []string) ([]string, []string) {
			args := append([]string{local(ws, "jest", "bin", "jest.js"), flagCI, "--setupFilesAfterEnv", filepath.Join(tools(ws), fuzzSetupCJS),
				flagJSON, "--outputFile=" + ws.Report(fuzzPrefix+JestReport)}, files...)
			return args, nil
		},
	},
	runnerMocha: {
		test: func(ws gate.Workspace) []string {
			return []string{toolScript(ws, "c8", "bin", "c8.js"), "--reporter=json", "--reports-dir=" + ws.Report(coverageDir), "--temp-directory=" + ws.Report(v8Dir),
				"--all", NodeBinary, local(ws, "mocha", "bin", "mocha.js"), flagReporter, reporterXUnit, flagReporterOpt, "output=" + ws.Report(JUnitReport)}
		},
		mutant: func(ws gate.Workspace) []string {
			return []string{local(ws, "mocha", "bin", "mocha.js"), flagBail, flagReporter, "dot"}
		},
		fuzz: func(ws gate.Workspace, files []string) ([]string, []string) {
			args := append([]string{local(ws, "mocha", "bin", "mocha.js"), flagReporter, reporterXUnit, flagReporterOpt, "output=" + ws.Report(fuzzPrefix+JUnitReport)}, files...)
			return args, []string{envNodeOptions + filepath.Join(tools(ws), fuzzSetupFile)}
		},
	},
	runnerNode: {
		test: func(ws gate.Workspace) []string {
			return []string{flagTest, "--experimental-test-coverage", flagTestJUnit, "--test-reporter-destination=" + ws.Report(JUnitReport),
				"--test-reporter=lcov", "--test-reporter-destination=" + ws.Report(lcovFile)}
		},
		mutant: func(gate.Workspace) []string { return []string{flagTest} },
		fuzz: func(ws gate.Workspace, files []string) ([]string, []string) {
			args := append([]string{flagTest, flagTestJUnit, "--test-reporter-destination=" + ws.Report(fuzzPrefix+JUnitReport)}, files...)
			return args, []string{envNodeOptions + filepath.Join(tools(ws), fuzzSetupFile)}
		},
	},
}

func project(ws gate.Workspace) (runner, error) {
	m, err := readManifest(stage(ws))
	return m.runner(), err
}

func withCoverageProvider(ctx context.Context, ws gate.Workspace, r runner) error {
	if r != runnerVitest {
		return nil
	}
	version, err := installedVersion(stage(ws), string(runnerVitest))
	if err != nil {
		return err
	}
	_, err = run(ctx, ws, stage(ws), nil, NPMBinary, "install", "--no-save", flagNoAudit, flagNoFund, "@vitest/coverage-v8@"+version)
	return err
}

func runTests(ctx context.Context, ws gate.Workspace) gate.Outcome {
	r, err := project(ws)
	if err == nil {
		err = withCoverageProvider(ctx, ws, r)
	}
	if err != nil {
		return gate.Fail(err.Error())
	}
	_, runErr := node(ctx, ws, nil, adapters[r].test(ws)...)
	cases, lerr := results(ws, r, "")
	if lerr != nil {
		return gate.Fail(reasonNoReport + ": " + gate.Tail(errors.Join(runErr, lerr).Error()))
	}
	if cerr := normalizeCoverage(ctx, ws); cerr != nil {
		return gate.Fail(cerr.Error())
	}
	return junit.Judge(cases, testID(ws))
}

func results(ws gate.Workspace, r runner, prefix string) ([]junit.Case, error) {
	if r == runnerJest {
		return jestCases(ws.Report(prefix + JestReport))
	}
	return junit.Load(ws.Report(prefix + JUnitReport))
}

func jestCases(file string) ([]junit.Case, error) {
	var rep struct {
		TestResults []struct {
			Name             string `json:"name"`
			Message          string `json:"message"`
			AssertionResults []struct {
				Ancestors []string `json:"ancestorTitles"`
				Title     string   `json:"title"`
				Status    string   `json:"status"`
				Failure   []string `json:"failureMessages"`
			} `json:"assertionResults"`
		} `json:"testResults"`
	}
	data, err := os.ReadFile(filepath.Clean(file))
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	var cases []junit.Case
	for _, suite := range rep.TestResults {
		for _, a := range suite.AssertionResults {
			c := junit.Case{File: suite.Name, Name: strings.Join(append(a.Ancestors, a.Title), idSeparator)}
			setStatus(&c, a.Status, strings.Join(a.Failure, "\n"))
			cases = append(cases, c)
		}
	}
	return cases, err
}

func setStatus(c *junit.Case, status, message string) {
	switch status {
	case "passed":
	case "failed":
		c.Failure = &junit.Outcome{Message: firstLine(message)}
	default:
		c.Skipped = &junit.Outcome{Message: status}
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func testID(ws gate.Workspace) func(junit.Case) string {
	return func(c junit.Case) string {
		if c.File != "" {
			return rel(ws, c.File) + idSeparator + c.Name
		}
		return c.Classname + idSeparator + c.Name
	}
}

func normalizeCoverage(ctx context.Context, ws gate.Workspace) error {
	cov := linecov.Report{}
	if err := readIstanbul(ws, cov); err != nil {
		return err
	}
	if err := readLcov(ws, cov); err != nil {
		return err
	}
	if err := addUnloaded(ctx, ws, cov); err != nil {
		return err
	}
	return cov.Save(ws)
}

func readIstanbul(ws gate.Workspace, cov linecov.Report) error {
	data, err := os.ReadFile(filepath.Join(ws.Report(coverageDir), istanbulFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var files map[string]struct {
		StatementMap map[string]struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"statementMap"`
		S map[string]int   `json:"s"`
		B map[string][]int `json:"b"`
	}
	if err == nil {
		err = json.Unmarshal(data, &files)
	}
	for name, f := range files {
		fc := cov.File(rel(ws, name))
		for id, st := range f.StatementMap {
			fc.Lines[st.Start.Line] = max(fc.Lines[st.Start.Line], f.S[id])
		}
		for _, counts := range f.B {
			fc.Branches += len(counts)
			fc.CoveredBranches += countPositive(counts)
		}
	}
	return err
}

func countPositive(values []int) int {
	n := 0
	for _, v := range values {
		if v > 0 {
			n++
		}
	}
	return n
}

func readLcov(ws gate.Workspace, cov linecov.Report) error {
	return readLcovFile(ws, ws.Report(lcovFile), cov)
}

func readLcovFile(ws gate.Workspace, path string, cov linecov.Report) error {
	f, err := os.Open(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var current *linecov.File
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		current = lcovLine(ws, cov, current, sc.Text())
	}
	return sc.Err()
}

func lcovLine(ws gate.Workspace, cov linecov.Report, current *linecov.File, line string) *linecov.File {
	key, value, _ := strings.Cut(line, ":")
	fields := strings.Split(value, ",")
	switch {
	case key == "SF":
		return cov.File(rel(ws, filepath.Join(stage(ws), value)))
	case current == nil || key == lcovEnd:
		return nil
	case key == "DA" && len(fields) >= 2:
		n, _ := strconv.Atoi(fields[0])
		hits, _ := strconv.Atoi(fields[1])
		current.Lines[n] = max(current.Lines[n], hits)
	case key == "BRDA" && len(fields) == 4:
		current.Branches++
		if taken, err := strconv.Atoi(fields[3]); err == nil && taken > 0 {
			current.CoveredBranches++
		}
	}
	return current
}

func addUnloaded(ctx context.Context, ws gate.Workspace, cov linecov.Report) error {
	files, err := productionFiles(ctx, ws)
	for _, f := range files {
		if cov[f] != nil {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(stage(ws), filepath.FromSlash(f)))
		if rerr != nil {
			return rerr
		}
		fc := cov.File(f)
		for _, line := range codeLines(f, data) {
			fc.Lines[line] = 0
		}
	}
	return err
}

func codeLines(name string, data []byte) []int {
	toks, _, _ := lexis.Lex(name, data)
	var lines []int
	for _, t := range toks {
		if !t.Note && !slices.Contains(lines, t.Line) {
			lines = append(lines, t.Line)
		}
	}
	return lines
}

func isTest(rel string) bool {
	base := path.Base(rel)
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasPrefix(rel, "test/") || strings.HasPrefix(rel, "tests/") ||
		strings.Contains(rel, "/test/") || strings.Contains(rel, "/tests/") || strings.Contains(rel, "__tests__/")
}

func isConfig(rel string) bool {
	return strings.Contains(path.Base(rel), ".config.")
}

func productionFiles(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return source.Select(ctx, ws, func(f string) bool {
		return slices.Contains(Sources, path.Ext(f)) && !isTest(f) && !isConfig(f) && !source.Fixture(f)
	})
}

func runFuzz(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := fuzzFiles(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(files) == 0 {
		return gate.Fail(reasonNoFuzz)
	}
	r, err := project(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	args, env := adapters[r].fuzz(ws, files)
	_, runErr := node(ctx, ws, append(env, envFuzzRuns+itoa(ws.Policy.Limits.FuzzExamples)), args...)
	cases, lerr := results(ws, r, fuzzPrefix)
	if lerr != nil {
		return gate.Fail(reasonNoReport + ": " + gate.Tail(errors.Join(runErr, lerr).Error()))
	}
	out := junit.Judge(cases, testID(ws))
	out.Values = map[string]float64{ValueFuzzFiles: float64(len(files))}
	if out.Status == gate.StatusFail {
		out.Reason = reasonFuzz
	}
	return out
}

func fuzzFiles(ctx context.Context, ws gate.Workspace) ([]string, error) {
	files, err := source.Select(ctx, ws, func(f string) bool { return isTest(f) && slices.Contains(Sources, path.Ext(f)) && !source.Fixture(f) })
	var out []string
	for _, f := range files {
		data, rerr := os.ReadFile(filepath.Join(ws.Src, filepath.FromSlash(f)))
		if rerr != nil {
			return nil, rerr
		}
		if strings.Contains(string(data), fastCheck) {
			out = append(out, f)
		}
	}
	return out, err
}

func Passed(ctx context.Context, ws gate.Workspace) (map[string]bool, error) {
	r, err := project(ws)
	if err != nil {
		return nil, err
	}
	cases, err := results(ws, r, "")
	if err != nil {
		return nil, err
	}
	return junit.Passed(cases, testID(ws)), nil
}
