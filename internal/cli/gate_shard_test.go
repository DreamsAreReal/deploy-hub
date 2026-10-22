package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	flagShard  = "--mutation-shard"
	flagShards = "--mutation-shards"
	hubEntry   = "  deploy-hub:\n    repo: DreamsAreReal/deploy-hub\n"
	checkTests = "tests"
)

func shardedPolicy(t *testing.T) string {
	t.Helper()
	text := testutil.Read(t, gatePolicy(t))
	testutil.True(t, strings.Contains(text, hubEntry), "the hub entry is in the policy")
	path := filepath.Join(t.TempDir(), "gate.yaml")
	testutil.Write(t, path, strings.Replace(text, hubEntry, hubEntry+"    mutation_shards: 3\n", 1))
	return path
}

type planned struct {
	plans []gate.MutationPlan
	ran   []string
}

func (p *planned) checks() []gate.Check {
	record := func(name string) func(context.Context, gate.Workspace) gate.Outcome {
		return func(_ context.Context, ws gate.Workspace) gate.Outcome {
			p.ran = append(p.ran, name)
			p.plans = append(p.plans, ws.Mutation)
			return gate.Pass(nil)
		}
	}
	return []gate.Check{{Name: checkTests, Run: record(checkTests)}, {Name: pack.CheckMutation, Run: record(pack.CheckMutation)}}
}

func shardRun(t *testing.T, policyPath string, extra ...string) (*harness, *planned, int) {
	t.Helper()
	h := newHarness(t)
	p := &planned{}
	h.gateChecks = p.checks()
	out := filepath.Join(t.TempDir(), "gate")
	code := h.run(append(append([]string{CmdGate}, gateFlags(hubApp, hubRepo, commitSha, policyPath, t.TempDir(), out)...), extra...)...)
	return h, p, code
}

func TestShardRunRunsOnlyTestsAndMutationTesting(t *testing.T) {
	h := newHarness(t)
	p := &planned{}
	h.gateChecks = append([]gate.Check{{Name: "lint", Run: func(context.Context, gate.Workspace) gate.Outcome { return gate.Pass(nil) }}}, p.checks()...)
	out := filepath.Join(t.TempDir(), "gate")
	code := h.run(append([]string{CmdGate, flagShard, "2/3"}, gateFlags(hubApp, hubRepo, commitSha, shardedPolicy(t), t.TempDir(), out)...)...)
	testutil.Equal(t, code, ExitOK)
	testutil.Equal(t, p.ran, []string{checkTests, pack.CheckMutation})
	testutil.Contains(t, h.out.String(), "(2 checks")
	testutil.Equal(t, p.plans[1], gate.MutationPlan{Shards: 3, Shard: 2, Subject: gate.Subject{App: hubApp, Repo: hubRepo, Commit: commitSha, Own: true}})
}

func TestMergeRunPassesTheShardReportsToEveryCheck(t *testing.T) {
	dir := t.TempDir()
	_, p, code := shardRun(t, shardedPolicy(t), flagShards, dir+"/")
	testutil.Equal(t, code, ExitOK)
	testutil.Equal(t, p.ran, []string{checkTests, pack.CheckMutation})
	testutil.Equal(t, p.plans[1].Merge, dir)
	testutil.Equal(t, p.plans[1].Shard, -1)
	testutil.Equal(t, p.plans[1].Shards, 3)
}

func TestUnshardedRunKeepsEveryCheck(t *testing.T) {
	_, p, code := shardRun(t, gatePolicy(t))
	testutil.Equal(t, code, ExitOK)
	testutil.Equal(t, p.ran, []string{checkTests, pack.CheckMutation})
	testutil.Equal(t, p.plans[0].Shards, 1)
	testutil.Equal(t, p.plans[0].Merge, "")
}

func TestBadShardSpecIsAUsageError(t *testing.T) {
	for _, spec := range []string{"x", "1", "3/3", "-1/3", "1/2", "a/3", "1/b"} {
		h, p, code := shardRun(t, shardedPolicy(t), flagShard, spec)
		testutil.Equal(t, code, ExitUsage)
		testutil.Equal(t, len(p.ran), 0)
		testutil.Contains(t, h.err.String(), ErrShardSpec.Error()+" (3)")
	}
	h, _, code := shardRun(t, gatePolicy(t), flagShard, "0/1")
	testutil.Equal(t, code, ExitUsage)
	testutil.Contains(t, h.err.String(), "(1)")
}

func TestShardFlagsAreValidated(t *testing.T) {
	src, out := t.TempDir(), filepath.Join(t.TempDir(), "o")
	base := gateFlags(testApp, testRepo, commitSha, "p", src, out)
	for _, extra := range [][]string{{flagShards, "relative"}, {flagShards, out, flagShard, "0/2"}} {
		_, err := parseGateArgs(append(append([]string{}, base...), extra...))
		testutil.Is(t, err, ErrGateArgs)
	}
	g, err := parseGateArgs(append(append([]string{}, base...), flagShard, "0/2"))
	testutil.NoErr(t, err)
	testutil.Equal(t, g.shard, "0/2")
}
