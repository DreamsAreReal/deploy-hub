package linecov

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	ReportFile        = "coverage.json"
	ValueLineCoverage = "line_coverage"
	ValueBranchCover  = "branch_coverage"
	ValueLines        = "lines"
	ValueBranches     = "branches"
	reasonLines       = "line coverage is below the policy threshold"
	reasonBranches    = "branch coverage is below the policy threshold"
	uncoveredForm     = "%s: %d of %d lines uncovered"
	percentScale      = 100
	reportPerm        = 0o600
)

type File struct {
	Lines           map[int]int `json:"lines"`
	Branches        int         `json:"branches"`
	CoveredBranches int         `json:"covered_branches"`
}

type Report map[string]*File

func (r Report) File(name string) *File {
	if r[name] == nil {
		r[name] = &File{Lines: map[int]int{}}
	}
	return r[name]
}

func (r Report) Covered(rel string, line int) (bool, []string) {
	f := r[rel]
	return f != nil && f.Lines[line] > 0, nil
}

func (r Report) Save(ws gate.Workspace) error {
	data, err := json.Marshal(r)
	if err == nil {
		err = os.WriteFile(ws.Report(ReportFile), data, reportPerm)
	}
	return err
}

func Load(ws gate.Workspace) (Report, error) {
	data, err := os.ReadFile(ws.Report(ReportFile))
	if err != nil {
		return nil, fmt.Errorf("coverage report missing: run the tests check first: %w", err)
	}
	r := Report{}
	return r, json.Unmarshal(data, &r)
}

func Ratio(part, whole int) float64 {
	if whole == 0 {
		return percentScale
	}
	return float64(part) * percentScale / float64(whole)
}

func Uncovered(name string, missing, lines int) string {
	return fmt.Sprintf(uncoveredForm, name, missing, lines)
}

func Lines(ws gate.Workspace, covered, total int, unit string, findings []string) gate.Outcome {
	pct := Ratio(covered, total)
	values := map[string]float64{ValueLineCoverage: pct, unit: float64(total)}
	return gate.Judge(pct >= ws.Policy.Thresholds.LineCoverage, reasonLines, values, findings)
}

func BranchOutcome(ws gate.Workspace, covered, total int) gate.Outcome {
	pct := Ratio(covered, total)
	values := map[string]float64{ValueBranchCover: pct, ValueBranches: float64(total)}
	return gate.Judge(pct >= ws.Policy.Thresholds.BranchCoverage, reasonBranches, values, nil)
}

func loaded(judge func(gate.Workspace, Report) gate.Outcome) func(context.Context, gate.Workspace) gate.Outcome {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		if err := ws.WaitFor(ctx, gate.CheckTests); err != nil {
			return gate.Fail(err.Error())
		}
		r, err := Load(ws)
		if err != nil {
			return gate.Fail(err.Error())
		}
		return judge(ws, r)
	}
}

var (
	LineCoverage = loaded(lineCoverage)
	Branches     = loaded(branches)
)

func lineCoverage(ws gate.Workspace, r Report) gate.Outcome {
	covered, total := 0, 0
	var findings []string
	for _, name := range slices.Sorted(maps.Keys(r)) {
		lines := r[name].Lines
		hit := 0
		for _, h := range lines {
			if h > 0 {
				hit++
			}
		}
		covered, total = covered+hit, total+len(lines)
		if hit < len(lines) {
			findings = append(findings, Uncovered(name, len(lines)-hit, len(lines)))
		}
	}
	return Lines(ws, covered, total, ValueLines, findings)
}

func branches(ws gate.Workspace, r Report) gate.Outcome {
	covered, total := 0, 0
	for _, f := range r {
		covered, total = covered+f.CoveredBranches, total+f.Branches
	}
	return BranchOutcome(ws, covered, total)
}
