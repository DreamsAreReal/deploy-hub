package linecov

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fileA = "src/a.js"
	fileB = "src/b.js"
)

func workspace(t *testing.T) gate.Workspace {
	t.Helper()
	return gate.Workspace{Out: t.TempDir(), Policy: policy.Policy{Thresholds: policy.Thresholds{LineCoverage: 75, BranchCoverage: 50}}}
}

func TestReportRoundTrip(t *testing.T) {
	ws := workspace(t)
	r := Report{}
	r.File(fileA).Lines[1] = 2
	r.File(fileA).Lines[2] = 0
	r.File(fileA).Branches, r.File(fileA).CoveredBranches = 4, 1
	testutil.NoErr(t, r.Save(ws))
	loaded, err := Load(ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, loaded, r)
	covered, tests := loaded.Covered(fileA, 1)
	testutil.True(t, covered && tests == nil, "a hit line is covered by every test")
	for _, line := range []int{2, 3} {
		covered, _ = loaded.Covered(fileA, line)
		testutil.True(t, !covered, "missed lines are not covered")
	}
	covered, _ = loaded.Covered(fileB, 1)
	testutil.True(t, !covered, "unknown files are not covered")
}

func TestLoadFailures(t *testing.T) {
	ws := workspace(t)
	_, err := Load(ws)
	testutil.Contains(t, err.Error(), "coverage report missing")
	testutil.Write(t, ws.Report(ReportFile), "{")
	_, err = Load(ws)
	testutil.Fails(t, err)
	testutil.Equal(t, LineCoverage(context.Background(), ws).Status, gate.StatusFail)
	testutil.Equal(t, Branches(context.Background(), ws).Status, gate.StatusFail)
	blocked := workspace(t)
	blocked.Out = filepath.Join(t.TempDir(), "missing")
	testutil.Fails(t, Report{}.Save(blocked))
}

func TestCoverageChecks(t *testing.T) {
	ws := workspace(t)
	r := Report{
		fileA: {Lines: map[int]int{1: 1, 2: 1, 3: 1, 4: 0}, Branches: 2, CoveredBranches: 1},
		fileB: {Lines: map[int]int{1: 3}},
	}
	testutil.NoErr(t, r.Save(ws))
	lines := LineCoverage(context.Background(), ws)
	testutil.Equal(t, lines.Status, gate.StatusPass)
	testutil.Equal(t, lines.Values, map[string]float64{ValueLineCoverage: 80, ValueLines: 5})
	testutil.Equal(t, lines.Findings, []string{"src/a.js: 1 of 4 lines uncovered"})
	branches := Branches(context.Background(), ws)
	testutil.Equal(t, branches.Status, gate.StatusPass)
	testutil.Equal(t, branches.Values, map[string]float64{ValueBranchCover: 50, ValueBranches: 2})
	r[fileB].Lines[2] = 0
	r[fileA].CoveredBranches = 0
	testutil.NoErr(t, r.Save(ws))
	low := LineCoverage(context.Background(), ws)
	testutil.Equal(t, low.Status, gate.StatusFail)
	testutil.Contains(t, low.Reason, "line coverage")
	testutil.Equal(t, Branches(context.Background(), ws).Status, gate.StatusFail)
	testutil.Equal(t, Ratio(0, 0), float64(percentScale))
	testutil.Equal(t, Lines(ws, 3, 4, "statements", nil).Values, map[string]float64{ValueLineCoverage: 75, "statements": 4})
	testutil.Equal(t, BranchOutcome(ws, 0, 0).Status, gate.StatusPass)
}
