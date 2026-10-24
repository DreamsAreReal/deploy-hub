package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	flagFull   = "--mutation-full"
	flagBudget = "--mutation-budget"
	flagLast   = "--mutation-last"
	cachePath  = "/c.json"
)

func TestParseGateArgsFullRun(t *testing.T) {
	src := t.TempDir()
	last := filepath.Join(t.TempDir(), "last.json")
	base := gateFlags(testApp, testRepo, commitSha, "p", src, filepath.Join(t.TempDir(), "out"))
	g, err := parseGateArgs(append(append([]string{}, base...), flagFull, flagBudget, "45m", flagLast, last))
	testutil.NoErr(t, err)
	testutil.True(t, g.full && g.budget == 45*time.Minute && g.last == last, "full run flags parsed")
	for _, extra := range [][]string{
		{flagBudget, "-1m"},
		{flagLast, "relative-last.json"},
		{flagLast, filepath.Join(src, "planted.json")},
	} {
		_, err := parseGateArgs(append(append([]string{}, base...), extra...))
		testutil.Fails(t, err)
	}
}

func TestMutationPlanReadsTheLastFullRun(t *testing.T) {
	last := filepath.Join(t.TempDir(), "last.json")
	testutil.Write(t, last, `{"commit":"abc","bases":["b"],"passed":true}`)
	g := gateArgs{last: last, full: true, budget: time.Minute, cache: cachePath, cacheSum: cacheSum}
	plan, err := g.mutationPlan()
	testutil.NoErr(t, err)
	testutil.Equal(t, *plan.Last, gate.FullRun{Commit: "abc", Bases: []string{"b"}, Passed: true})
	testutil.True(t, plan.Full && plan.Budget == time.Minute && plan.Cache == cachePath && plan.CacheSHA256 == cacheSum[len(digestPrefix):], "plan carries the flags")
	testutil.Write(t, last, "{")
	_, err = g.mutationPlan()
	testutil.Fails(t, err)
	g.last = filepath.Join(t.TempDir(), "missing.json")
	_, err = g.mutationPlan()
	testutil.Fails(t, err)
}

func TestFullRunKeepsOnlyTestsAndMutation(t *testing.T) {
	all := []gate.Check{{Name: "lint"}, {Name: pack.CheckTests}, {Name: pack.CheckMutation}, {Name: "secrets"}}
	testutil.Equal(t, len(gateChecks(all, false)), 4)
	names := make([]string, 0, len(all))
	for _, c := range gateChecks(all, true) {
		names = append(names, c.Name)
	}
	testutil.Equal(t, names, []string{pack.CheckTests, pack.CheckMutation})
	testutil.Equal(t, len(all), 4)
}

func TestGateCommandSummarizesTheMutationRun(t *testing.T) {
	h := newHarness(t)
	h.gateChecks = []gate.Check{{Name: pack.CheckMutation, Run: func(_ context.Context, ws gate.Workspace) gate.Outcome {
		data, err := json.Marshal(mutation.ProjectRun{Base: verdictKey, Complete: true, Passed: true, Fresh: true})
		testutil.NoErr(t, err)
		testutil.Write(t, filepath.Join(ws.Out, "go-app", mutation.RunFile), string(data))
		return gate.Pass(nil)
	}}}
	out := filepath.Join(t.TempDir(), "gate")
	testutil.Equal(t, h.run(append([]string{CmdGate}, append(gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), out), flagFull)...)...), ExitOK)
	var s mutation.RunSummary
	testutil.NoErr(t, json.Unmarshal([]byte(testutil.Read(t, filepath.Join(out, mutation.RunFile))), &s))
	testutil.Equal(t, s, mutation.RunSummary{Complete: true, Passed: true, Fresh: true, Stale: true, Bases: []string{verdictKey}})
	testutil.Contains(t, h.out.String(), "complete=true passed=true fresh=true stale=true")
	h2 := newHarness(t)
	h2.gateChecks = []gate.Check{{Name: pack.CheckMutation, Run: func(_ context.Context, ws gate.Workspace) gate.Outcome {
		testutil.Write(t, filepath.Join(ws.Out, "go-app", mutation.RunFile), "{")
		return gate.Pass(nil)
	}}}
	testutil.Equal(t, h2.run(append([]string{CmdGate}, gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), filepath.Join(t.TempDir(), "g"))...)...), ExitFail)
	h3 := newHarness(t)
	bad := filepath.Join(t.TempDir(), "last.json")
	testutil.Write(t, bad, "{")
	testutil.Equal(t, h3.run(append([]string{CmdGate}, append(gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), filepath.Join(t.TempDir(), "g")), flagLast, bad)...)...), ExitFail)
}
