package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
)

const (
	msgRunSummary   = "mutation run: %s complete=%t passed=%t fresh=%t stale=%t\n"
	filePermReports = 0o600
)

func (g *gateArgs) fullRunArgs() error {
	if g.budget < 0 || (g.last != "" && (!filepath.IsAbs(g.last) || !outside(g.src, filepath.Clean(g.last)))) {
		return fmt.Errorf("%w: --mutation-budget must not be negative and --mutation-last needs an absolute path outside the source tree", ErrGateArgs)
	}
	return nil
}

func (g *gateArgs) mutationPlan() (gate.MutationPlan, error) {
	plan := gate.MutationPlan{Full: g.full, Budget: g.budget}
	if g.cache != "" {
		plan.Cache, plan.CacheSHA256 = filepath.Clean(g.cache), g.cacheSum[len(digestPrefix):]
	}
	if g.last == "" {
		return plan, nil
	}
	data, err := os.ReadFile(filepath.Clean(g.last))
	if err == nil {
		plan.Last = &gate.FullRun{}
		err = json.Unmarshal(data, plan.Last)
	}
	if err != nil {
		return plan, fmt.Errorf("--mutation-last: %w", err)
	}
	return plan, nil
}

func gateChecks(all []gate.Check, full bool) []gate.Check {
	if !full {
		return all
	}
	return slices.DeleteFunc(slices.Clone(all), func(c gate.Check) bool { return c.Name != pack.CheckTests && c.Name != pack.CheckMutation })
}

func projectFiles(out, name string) ([]string, error) {
	entries, err := os.ReadDir(out)
	var found []string
	for _, e := range entries {
		if p := filepath.Join(out, e.Name(), name); e.IsDir() && exists(p) {
			found = append(found, p)
		}
	}
	return found, err
}

func (a *App) keepVerdicts(ws gate.Workspace) error {
	projects, err := projectFiles(ws.Out, mutation.VerdictsFile)
	if err != nil || len(projects) == 0 {
		return err
	}
	out := ws.Report(mutation.VerdictsFile)
	digest, err := mutation.MergeVerdicts(out, projects)
	if err == nil {
		a.Out.Printf(msgVerdicts, out, digestPrefix, digest)
	}
	return err
}

func (a *App) keepRunSummary(ws gate.Workspace) error {
	projects, err := projectFiles(ws.Out, mutation.RunFile)
	if err != nil || len(projects) == 0 {
		return err
	}
	s, err := mutation.SummarizeRuns(projects, ws.Mutation.Last)
	var data []byte
	if err == nil {
		data, err = json.Marshal(s)
	}
	out := ws.Report(mutation.RunFile)
	if err == nil {
		err = os.WriteFile(out, data, filePermReports)
	}
	if err == nil {
		a.Out.Printf(msgRunSummary, out, s.Complete, s.Passed, s.Fresh, s.Stale)
	}
	return err
}
