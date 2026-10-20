package dotnet

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
)

const (
	TRXReport         = "results.trx"
	CoverageReport    = "coverage.json"
	resultsDir        = "results"
	coberturaFile     = "coverage.cobertura.xml"
	fuzzPrefix        = "fuzz-"
	ValueLineCoverage = "line_coverage"
	ValueBranchCover  = "branch_coverage"
	ValueLines        = "lines"
	ValueBranches     = "branches"
	percentScale      = 100
	outcomePassed     = "Passed"
	outcomeFailed     = "Failed"
	envFuzzBudget     = "CsCheck_Iter="
	reasonLines       = "line coverage is below the policy threshold"
	reasonBranches    = "branch coverage is below the policy threshold"
	reasonFuzz        = "property-based fuzz tests failed"
	reasonNoFuzz      = "no fuzz targets: tests must use a property-based library (CsCheck or FsCheck)"
	reasonNoReport    = "dotnet test produced no report"
	uncoveredForm     = "%s: %d of %d lines uncovered"
	coveragePerm      = 0o600
	sexagesimal       = 60
)

var (
	quietAnalyzers    = []string{"-p:RunAnalyzers=false", "-p:TreatWarningsAsErrors=false"}
	conditionCoverage = regexp.MustCompile(`\((\d+)/(\d+)\)`)
	propertyLibraries = []string{"cscheck", "fscheck"}
)

type trx struct {
	Results []struct {
		TestName string `xml:"testName,attr"`
		Outcome  string `xml:"outcome,attr"`
		Duration string `xml:"duration,attr"`
		Message  string `xml:"Output>ErrorInfo>Message"`
	} `xml:"Results>UnitTestResult"`
}

func loadTRX(file string) ([]junit.Case, error) {
	data, err := os.ReadFile(filepath.Clean(file))
	var report trx
	if err == nil {
		err = xml.Unmarshal(data, &report)
	}
	cases := make([]junit.Case, 0, len(report.Results))
	for _, r := range report.Results {
		c := junit.Case{Name: r.TestName, Time: seconds(r.Duration)}
		switch r.Outcome {
		case outcomePassed:
		case outcomeFailed:
			c.Failure = &junit.Outcome{Message: firstLine(r.Message)}
		default:
			c.Skipped = &junit.Outcome{Message: r.Outcome}
		}
		cases = append(cases, c)
	}
	return cases, err
}

func seconds(duration string) float64 {
	parts := strings.Split(duration, ":")
	total := 0.0
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		total = total*sexagesimal + v
	}
	return total
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func testName(c junit.Case) string {
	return c.Name
}

func testProps(ws gate.Workspace) []string {
	return append(analysisProps(ws), quietAnalyzers...)
}

func Tests(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	args := append([]string{cmdTest, flagNoRestore, "--results-directory", ws.Report(resultsDir), flagLogger, "trx;LogFileName=" + ws.Report(TRXReport),
		"--collect:XPlat Code Coverage"}, testProps(ws)...)
	_, runErr := run(ctx, ws, stage(ws), nil, args...)
	cases, lerr := loadTRX(ws.Report(TRXReport))
	if lerr != nil {
		return gate.Fail(reasonNoReport + ": " + gate.Tail(errors.Join(runErr, lerr).Error()))
	}
	if cerr := normalizeCoverage(ctx, ws); cerr != nil {
		return gate.Fail(cerr.Error())
	}
	return junit.Judge(cases, testName)
}

type fileCoverage struct {
	Lines           map[int]int `json:"lines"`
	Branches        int         `json:"branches"`
	CoveredBranches int         `json:"covered_branches"`
}

type cobertura struct {
	Sources []string         `xml:"sources>source"`
	Classes []coberturaClass `xml:"packages>package>classes>class"`
}

func (c cobertura) path(filename string) string {
	for _, src := range c.Sources {
		if full := filepath.Join(src, filename); exists(full) {
			return full
		}
	}
	return filename
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func normalizeCoverage(_ context.Context, ws gate.Workspace) error {
	cov := map[string]*fileCoverage{}
	err := filepath.WalkDir(ws.Report(resultsDir), func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || d.Name() != coberturaFile {
			return werr
		}
		return mergeCobertura(ws, p, cov)
	})
	if err != nil {
		return err
	}
	data, err := json.Marshal(cov)
	if err == nil {
		err = os.WriteFile(ws.Report(CoverageReport), data, coveragePerm)
	}
	return err
}

type branchKey struct {
	file string
	line int
}

type coberturaClass = struct {
	Filename string `xml:"filename,attr"`
	Lines    []struct {
		Number    int    `xml:"number,attr"`
		Hits      int    `xml:"hits,attr"`
		Condition string `xml:"condition-coverage,attr"`
	} `xml:"lines>line"`
}

func mergeCobertura(ws gate.Workspace, file string, cov map[string]*fileCoverage) error {
	data, err := os.ReadFile(filepath.Clean(file))
	var report cobertura
	if err == nil {
		err = xml.Unmarshal(data, &report)
	}
	seen := map[branchKey]bool{}
	for _, c := range report.Classes {
		mergeClass(rel(ws, report.path(c.Filename)), c, cov, seen)
	}
	return err
}

func mergeClass(name string, c coberturaClass, cov map[string]*fileCoverage, seen map[branchKey]bool) {
	fc := cov[name]
	if fc == nil {
		fc = &fileCoverage{Lines: map[int]int{}}
		cov[name] = fc
	}
	for _, l := range c.Lines {
		fc.Lines[l.Number] = max(fc.Lines[l.Number], l.Hits)
		m := conditionCoverage.FindStringSubmatch(l.Condition)
		if m == nil || seen[branchKey{name, l.Number}] {
			continue
		}
		seen[branchKey{name, l.Number}] = true
		covered, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		fc.CoveredBranches, fc.Branches = fc.CoveredBranches+covered, fc.Branches+total
	}
}

func loadCoverage(ws gate.Workspace) (map[string]*fileCoverage, error) {
	cov := map[string]*fileCoverage{}
	data, err := os.ReadFile(ws.Report(CoverageReport))
	if err != nil {
		return nil, fmt.Errorf("coverage report missing: run the tests check first: %w", err)
	}
	return cov, json.Unmarshal(data, &cov)
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return percentScale
	}
	return float64(part) * percentScale / float64(whole)
}

func LineCoverage(_ context.Context, ws gate.Workspace) gate.Outcome {
	cov, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	names := make([]string, 0, len(cov))
	for name := range cov {
		names = append(names, name)
	}
	slices.Sort(names)
	covered, total := 0, 0
	var findings []string
	for _, name := range names {
		hit := hitLines(cov[name].Lines)
		lines := len(cov[name].Lines)
		covered, total = covered+hit, total+lines
		if hit < lines {
			findings = append(findings, fmt.Sprintf(uncoveredForm, name, lines-hit, lines))
		}
	}
	pct := ratio(covered, total)
	return gate.Judge(pct >= ws.Policy.Thresholds.LineCoverage, reasonLines, map[string]float64{ValueLineCoverage: pct, ValueLines: float64(total)}, findings)
}

func hitLines(lines map[int]int) int {
	hit := 0
	for _, h := range lines {
		if h > 0 {
			hit++
		}
	}
	return hit
}

func Branches(_ context.Context, ws gate.Workspace) gate.Outcome {
	cov, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	covered, total := 0, 0
	for _, f := range cov {
		covered, total = covered+f.CoveredBranches, total+f.Branches
	}
	pct := ratio(covered, total)
	return gate.Judge(pct >= ws.Policy.Thresholds.BranchCoverage, reasonBranches, map[string]float64{ValueBranchCover: pct, ValueBranches: float64(total)}, nil)
}

func Fuzz(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if err := prepare(ctx, ws); err != nil {
		return gate.Fail(err.Error())
	}
	locks, err := trackedFiles(ctx, ws, func(f string) bool { return path.Base(f) == lockFile })
	if err != nil {
		return gate.Fail(err.Error())
	}
	names, err := lockedNames(ws.Src, locks)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if !slices.ContainsFunc(names, func(n string) bool { return slices.Contains(propertyLibraries, strings.ToLower(n)) }) {
		return gate.Fail(reasonNoFuzz)
	}
	report := ws.Report(fuzzPrefix + TRXReport)
	args := append([]string{cmdTest, flagNoRestore, flagLogger, "trx;LogFileName=" + report}, testProps(ws)...)
	_, runErr := run(ctx, ws, stage(ws), []string{envFuzzBudget + itoa(ws.Policy.Limits.FuzzExamples)}, args...)
	cases, lerr := loadTRX(report)
	if lerr != nil {
		return gate.Fail(reasonNoReport + ": " + gate.Tail(errors.Join(runErr, lerr).Error()))
	}
	out := junit.Judge(cases, testName)
	if out.Status == gate.StatusFail {
		out.Reason = reasonFuzz
	}
	return out
}

func Passed(_ context.Context, ws gate.Workspace) (map[string]bool, error) {
	cases, err := loadTRX(ws.Report(TRXReport))
	if err != nil {
		return nil, err
	}
	return junit.Passed(cases, testName), nil
}
