package pack

import (
	"context"
	"fmt"
	"maps"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
)

const (
	ValueSecurityScenarios = "security_scenarios_percent"
	ValueBusinessScenarios = "business_scenarios_percent"
	ValueScenarios         = "scenarios"
	reasonScenarios        = "mandatory scenarios are not covered by passing tests"
	scenarioFormat         = "%s (%s): test %s did not pass"
	percentScale           = 100
)

func Scenarios(packs []Pack) Run {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		passed, err := passedTests(ctx, ws, packs)
		if err != nil {
			return gate.Fail(err.Error())
		}
		tally := map[string]*scenarioTally{policy.CategorySecurity: {}, policy.CategoryBusiness: {}}
		var findings []string
		for _, s := range ws.App.Scenarios {
			missing := missingTests(passed, s.Tests)
			for _, m := range missing {
				findings = append(findings, fmt.Sprintf(scenarioFormat, s.ID, s.Category, m))
			}
			tally[s.Category].add(len(missing) == 0)
		}
		values := map[string]float64{
			ValueSecurityScenarios: tally[policy.CategorySecurity].percent(),
			ValueBusinessScenarios: tally[policy.CategoryBusiness].percent(),
			ValueScenarios:         float64(len(ws.App.Scenarios)),
		}
		return gate.Judge(len(findings) == 0 && len(ws.App.Scenarios) > 0, reasonScenarios, values, findings)
	}
}

func passedTests(ctx context.Context, ws gate.Workspace, packs []Pack) (map[string]bool, error) {
	layout, err := layoutOf(ctx, ws, packs)
	if err != nil {
		return nil, err
	}
	passed := map[string]bool{}
	for _, p := range layout.Projects {
		pws, werr := p.Workspace(ws)
		if werr != nil {
			return nil, werr
		}
		ids, perr := p.Pack.Passed(ctx, pws)
		if perr != nil {
			return nil, fmt.Errorf("%s: %w", p.Label(), perr)
		}
		maps.Copy(passed, ids)
	}
	return passed, nil
}

func missingTests(passed map[string]bool, tests []string) []string {
	var out []string
	for _, t := range tests {
		if !passed[t] {
			out = append(out, t)
		}
	}
	return out
}

type scenarioTally struct {
	covered, total int
}

func (t *scenarioTally) add(covered bool) {
	t.total++
	if covered {
		t.covered++
	}
}

func (t *scenarioTally) percent() float64 {
	if t.total == 0 {
		return percentScale
	}
	return float64(t.covered) * percentScale / float64(t.total)
}
