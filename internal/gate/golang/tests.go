package golang

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	TestsReport    = "tests.json"
	CoverReport    = "cover.out"
	ValueTests     = "tests"
	ValueFailed    = "failed"
	ValueSkipped   = "skipped"
	ValuePackages  = "packages"
	actionPass     = "pass"
	actionFail     = "fail"
	actionSkip     = "skip"
	actionBuild    = "build-fail"
	testSeparator  = "."
	reportPerm     = 0o600
	maxEventLine   = 4 << 20
	reasonTests    = "tests failed or were skipped"
	failedFormat   = "%s %s failed"
	skippedFormat  = "%s %s skipped"
	pkgFailFormat  = "%s: package failed (build error, panic or TestMain exit)"
	buildFailForm  = "%s: build failed"
	reportMissing  = "test report missing: run the tests check first: %w"
	coverPkgPrefix = "-coverpkg="
)

type event struct {
	Action     string
	Package    string
	Test       string
	ImportPath string
}

type TestRun struct {
	Passed   map[string]bool
	Failures []string
	Tests    int
	Failed   int
	Skipped  int
}

func Tests(ctx context.Context, ws gate.Workspace) gate.Outcome {
	pkgs, err := List(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	args := []string{"test", flagJSON, "-race", "-count=1", "-timeout=" + ws.Policy.Limits.Check.String(), "-covermode=atomic", coverPkgPrefix + strings.Join(pkgs.Production().Paths(), ","), "-coverprofile=" + ws.Report(CoverReport), allPkgs}
	res, runErr := ws.Run(ctx, GoBinary, args...)
	if werr := os.WriteFile(ws.Report(TestsReport), []byte(res.Stdout), reportPerm); werr != nil {
		return gate.Fail("cannot keep the test report: " + werr.Error())
	}
	run, err := ParseTestEvents(res.Stdout)
	if err != nil {
		return gate.Fail("test report unreadable: " + err.Error())
	}
	values := map[string]float64{ValueTests: float64(run.Tests), ValueFailed: float64(run.Failed), ValueSkipped: float64(run.Skipped), ValuePackages: float64(len(pkgs))}
	if runErr != nil && len(run.Failures) == 0 {
		return gate.Fail("go test failed: " + gate.Tail(res.Combined()))
	}
	return gate.Judge(len(run.Failures) == 0 && run.Tests > 0, reasonTests, values, run.Failures)
}

func ParseTestEvents(stream string) (TestRun, error) {
	run := TestRun{Passed: map[string]bool{}}
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxEventLine)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return run, err
		}
		run.add(e)
	}
	return run, sc.Err()
}

func (r *TestRun) add(e event) {
	if e.Test == "" {
		r.addPackage(e)
		return
	}
	switch e.Action {
	case actionPass:
		r.Tests++
		r.Passed[e.Package+testSeparator+e.Test] = true
	case actionFail:
		r.Tests++
		r.Failed++
		r.Failures = append(r.Failures, fmt.Sprintf(failedFormat, e.Package, e.Test))
	case actionSkip:
		r.Tests++
		r.Skipped++
		r.Failures = append(r.Failures, fmt.Sprintf(skippedFormat, e.Package, e.Test))
	}
}

func (r *TestRun) addPackage(e event) {
	switch e.Action {
	case actionFail:
		r.Failures = append(r.Failures, fmt.Sprintf(pkgFailFormat, e.Package))
	case actionBuild:
		r.Failures = append(r.Failures, fmt.Sprintf(buildFailForm, e.ImportPath))
	}
}

func LoadTestRun(ws gate.Workspace) (TestRun, error) {
	data, err := os.ReadFile(filepath.Clean(ws.Report(TestsReport)))
	if err != nil {
		return TestRun{}, fmt.Errorf(reportMissing, err)
	}
	return ParseTestEvents(string(data))
}
