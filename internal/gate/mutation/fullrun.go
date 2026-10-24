package mutation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	RunFile          = "mutation-run.json"
	reasonNoFull     = "verdicts came from the cache, but no full mutation run has finished yet"
	reasonFullFailed = "verdicts came from the cache, but the last full mutation run (%s) is below the policy threshold"
	reasonStale      = "verdicts came from the cache, but go.mod, go.sum, the Go toolchain, the policy, the host or the engine changed since the last full mutation run"
)

type ProjectRun struct {
	Base     string  `json:"base,omitempty"`
	Complete bool    `json:"complete"`
	Passed   bool    `json:"passed"`
	Fresh    bool    `json:"fresh"`
	Score    float64 `json:"score"`
}

type RunSummary struct {
	Complete bool     `json:"complete"`
	Passed   bool     `json:"passed"`
	Fresh    bool     `json:"fresh"`
	Stale    bool     `json:"stale"`
	Bases    []string `json:"bases"`
}

func fullRunGap(plan gate.MutationPlan, rep Report) string {
	if plan.Full || rep.Cached == 0 {
		return ""
	}
	switch last := plan.Last; {
	case last == nil:
		return reasonNoFull
	case !last.Passed:
		return fmt.Sprintf(reasonFullFailed, last.Commit)
	case !slices.Contains(last.Bases, rep.Base):
		return reasonStale
	}
	return ""
}

func keepRun(ws gate.Workspace, rep Report, err error, out gate.Outcome) {
	pr := ProjectRun{Base: rep.Base, Complete: err == nil, Passed: out.Status == gate.StatusPass, Fresh: ws.Mutation.Full || rep.Cached == 0, Score: rep.Score()}
	data, merr := json.Marshal(pr)
	if merr == nil {
		merr = os.WriteFile(ws.Report(RunFile), data, filePerm)
	}
	if merr != nil && ws.Log != nil {
		ws.Log.Error("mutation run summary not kept", "error", merr.Error())
	}
}

func SummarizeRuns(paths []string, last *gate.FullRun) (RunSummary, error) {
	s := RunSummary{Complete: len(paths) > 0, Passed: len(paths) > 0, Fresh: true}
	for _, p := range paths {
		var pr ProjectRun
		data, err := os.ReadFile(filepath.Clean(p))
		if err == nil {
			err = json.Unmarshal(data, &pr)
		}
		if err != nil {
			return RunSummary{}, err
		}
		s = s.add(pr, last)
	}
	slices.Sort(s.Bases)
	s.Bases = slices.Compact(s.Bases)
	return s, nil
}

func (s RunSummary) add(pr ProjectRun, last *gate.FullRun) RunSummary {
	s.Complete = s.Complete && pr.Complete
	s.Passed = s.Passed && pr.Passed
	s.Fresh = s.Fresh && pr.Fresh
	if pr.Base == "" {
		return s
	}
	s.Bases = append(s.Bases, pr.Base)
	s.Stale = s.Stale || last == nil || !slices.Contains(last.Bases, pr.Base)
	return s
}
