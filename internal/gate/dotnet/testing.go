package dotnet

import (
	"context"
	"encoding/xml"
	"errors"
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
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	TRXReport      = "results.trx"
	resultsDir     = "results"
	coberturaFile  = "coverage.cobertura.xml"
	fuzzPrefix     = "fuzz-"
	outcomePassed  = "Passed"
	outcomeFailed  = "Failed"
	envFuzzBudget  = "CsCheck_Iter="
	reasonFuzz     = "property-based fuzz tests failed"
	reasonNoFuzz   = "no fuzz targets: tests must use a property-based library (CsCheck or FsCheck)"
	reasonNoReport = "dotnet test produced no report"
	sexagesimal    = 60
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

func runTests(ctx context.Context, ws gate.Workspace) gate.Outcome {
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
	cov := linecov.Report{}
	branches := map[branchKey][2]int{}
	err := filepath.WalkDir(ws.Report(resultsDir), func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || d.Name() != coberturaFile {
			return werr
		}
		return mergeCobertura(ws, p, cov, branches)
	})
	if err != nil {
		return err
	}
	for key, counts := range branches {
		fc := cov[key.file]
		fc.CoveredBranches, fc.Branches = fc.CoveredBranches+counts[0], fc.Branches+counts[1]
	}
	return cov.Save(ws)
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

func mergeCobertura(ws gate.Workspace, file string, cov linecov.Report, branches map[branchKey][2]int) error {
	data, err := os.ReadFile(filepath.Clean(file))
	var report cobertura
	if err == nil {
		err = xml.Unmarshal(data, &report)
	}
	for _, c := range report.Classes {
		mergeClass(rel(ws, report.path(c.Filename)), c, cov, branches)
	}
	return err
}

func mergeClass(name string, c coberturaClass, cov linecov.Report, branches map[branchKey][2]int) {
	fc := cov.File(name)
	for _, l := range c.Lines {
		fc.Lines[l.Number] = max(fc.Lines[l.Number], l.Hits)
		m := conditionCoverage.FindStringSubmatch(l.Condition)
		if m == nil {
			continue
		}
		covered, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		key := branchKey{name, l.Number}
		branches[key] = [2]int{max(branches[key][0], covered), max(branches[key][1], total)}
	}
}

func runFuzz(ctx context.Context, ws gate.Workspace) gate.Outcome {
	locks, err := source.Select(ctx, ws, func(f string) bool { return path.Base(f) == lockFile })
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
