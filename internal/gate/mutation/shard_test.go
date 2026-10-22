package mutation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	shardApp    = "tasks"
	shardRepo   = "o/tasks"
	shardCommit = "abc"
	shardSub    = "go-root"
	twoShards   = 2
)

func shardPlan(root string) gate.MutationPlan {
	return gate.MutationPlan{Shards: twoShards, Shard: -1, Merge: root, Sub: shardSub, Subject: gate.Subject{App: shardApp, Repo: shardRepo, Commit: shardCommit}}
}

func writeShard(t *testing.T, root string, k int, s shardReport, v gate.Verdict) {
	t.Helper()
	dir := filepath.Join(root, ShardDir(k))
	data, err := json.Marshal(s)
	testutil.NoErr(t, err)
	testutil.Write(t, filepath.Join(dir, shardSub, ShardFile), string(data))
	data, err = json.Marshal(v)
	testutil.NoErr(t, err)
	testutil.Write(t, filepath.Join(dir, shardVerdict), string(data))
}

func shardEntries(k, total int, status Status) []entry {
	var out []entry
	for i := k; i < total; i += twoShards {
		out = append(out, entry{ID: i, File: fileLib, Line: i + 1, Operator: "negate", Original: "a", Mutated: "b", Status: status})
	}
	return out
}

func subject() gate.Verdict {
	return gate.Verdict{Subject: gate.Subject{App: shardApp, Repo: shardRepo, Commit: shardCommit}}
}

func TestMergeJoinsEveryShardInMutantOrder(t *testing.T) {
	root := t.TempDir()
	writeShard(t, root, 0, shardReport{Shard: 0, Shards: 2, Total: 5, Entries: shardEntries(0, 5, StatusKilled)}, subject())
	writeShard(t, root, 1, shardReport{Shard: 1, Shards: 2, Total: 5, Entries: shardEntries(1, 5, StatusSurvived)}, subject())
	rep, err := Merge(shardPlan(root))
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[Status]int{StatusKilled: 3, StatusSurvived: 2})
	testutil.Equal(t, rep.Total, 5)
	for i, e := range rep.Entries {
		testutil.Equal(t, e.ID, i)
	}
	testutil.Equal(t, rep.Survivors, []string{"lib.go:2 negate: a -> b (survived)", "lib.go:4 negate: a -> b (survived)"})
	testutil.Equal(t, rep.Score(), float64(60))
}

func TestMergeRejectsShardsThatDoNotAddUp(t *testing.T) {
	cases := map[string]struct {
		second  shardReport
		verdict gate.Verdict
		want    error
		text    string
	}{
		"other commit":    {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: shardEntries(1, 4, StatusKilled)}, gate.Verdict{Subject: gate.Subject{App: shardApp, Repo: shardRepo, Commit: "zzz"}}, ErrIncomplete, "shard 1 ran on o/tasks tasks@zzz, not on o/tasks tasks@abc"},
		"other app":       {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: shardEntries(1, 4, StatusKilled)}, gate.Verdict{Subject: gate.Subject{App: "x", Repo: shardRepo, Commit: shardCommit}}, ErrIncomplete, "x@abc"},
		"other repo":      {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: shardEntries(1, 4, StatusKilled)}, gate.Verdict{Subject: gate.Subject{App: shardApp, Repo: "o/x", Commit: shardCommit}}, ErrIncomplete, "o/x tasks@abc"},
		"wrong index":     {shardReport{Shard: 0, Shards: 2, Total: 4, Entries: shardEntries(1, 4, StatusKilled)}, subject(), ErrShards, "shard 1 holds shard 0 of 2 over 4 mutants, expected shard 1 of 2 over 4"},
		"wrong count":     {shardReport{Shard: 1, Shards: 3, Total: 4, Entries: shardEntries(1, 4, StatusKilled)}, subject(), ErrShards, "holds shard 1 of 3"},
		"other total":     {shardReport{Shard: 1, Shards: 2, Total: 6, Entries: shardEntries(1, 6, StatusKilled)}, subject(), ErrShards, "over 6 mutants"},
		"foreign mutant":  {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: shardEntries(0, 4, StatusKilled)}, subject(), ErrShards, "shard 1 reports mutant 0 of 4"},
		"repeated mutant": {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: append(shardEntries(1, 4, StatusKilled), entry{ID: 1})}, subject(), ErrShards, "reports mutant 1 of 4"},
		"beyond total":    {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: []entry{{ID: 5}}}, subject(), ErrShards, "reports mutant 5 of 4"},
		"negative id":     {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: []entry{{ID: -1}}}, subject(), ErrShards, "reports mutant -1 of 4"},
		"missing mutants": {shardReport{Shard: 1, Shards: 2, Total: 4, Entries: shardEntries(1, 2, StatusKilled)}, subject(), ErrIncomplete, "the shards report 3 of 4 mutants"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeShard(t, root, 0, shardReport{Shard: 0, Shards: 2, Total: 4, Entries: shardEntries(0, 4, StatusKilled)}, subject())
			writeShard(t, root, 1, c.second, c.verdict)
			_, err := Merge(shardPlan(root))
			testutil.Is(t, err, c.want)
			testutil.Contains(t, err.Error(), c.text)
		})
	}
}

func TestMergeNeedsEveryShardReport(t *testing.T) {
	root := t.TempDir()
	writeShard(t, root, 0, shardReport{Shard: 0, Shards: 2, Total: 2, Entries: shardEntries(0, 2, StatusKilled)}, subject())
	_, err := Merge(shardPlan(root))
	testutil.Is(t, err, ErrIncomplete)
	testutil.Contains(t, err.Error(), "shard 1:")
	testutil.Write(t, filepath.Join(root, ShardDir(1), shardSub, ShardFile), "{")
	_, err = Merge(shardPlan(root))
	testutil.Is(t, err, ErrIncomplete)
	writeShard(t, root, 1, shardReport{Shard: 1, Shards: 2, Total: 2, Entries: shardEntries(1, 2, StatusKilled)}, subject())
	testutil.NoErr(t, os.Remove(filepath.Join(root, ShardDir(1), shardVerdict)))
	_, err = Merge(shardPlan(root))
	testutil.Is(t, err, ErrIncomplete)
	testutil.Write(t, filepath.Join(root, ShardDir(1), shardVerdict), "[")
	_, err = Merge(shardPlan(root))
	testutil.Is(t, err, ErrIncomplete)
}

func TestShardedDecidesHowMutationRuns(t *testing.T) {
	ran := 0
	run := Sharded(func(context.Context, gate.Workspace) gate.Outcome {
		ran++
		return gate.Pass(nil)
	})
	ws := gate.Workspace{Out: t.TempDir(), Policy: policy.Policy{Thresholds: policy.Thresholds{MutationScore: 90}}}
	testutil.Equal(t, run(context.Background(), ws).Status, gate.StatusPass)
	ws.Mutation = gate.MutationPlan{Shards: 3, Shard: 1}
	testutil.Equal(t, run(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, ran, 2)
	ws.Mutation = gate.MutationPlan{Shards: 3, Shard: -1}
	out := run(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, "the policy runs mutation testing in 3 shards; the gate needs their reports")
	root := t.TempDir()
	writeShard(t, root, 0, shardReport{Shard: 0, Shards: 2, Total: 2, Entries: shardEntries(0, 2, StatusKilled)}, subject())
	writeShard(t, root, 1, shardReport{Shard: 1, Shards: 2, Total: 2, Entries: shardEntries(1, 2, StatusSurvived)}, subject())
	ws.Mutation = shardPlan(root)
	ws.Policy.Thresholds.MutationScore = 50
	out = run(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values[ValueScore], float64(50))
	testutil.Equal(t, ran, 2)
	testutil.Contains(t, testutil.Read(t, filepath.Join(ws.Out, ReportFile)), `"id":1`)
	ws.Mutation.Merge = filepath.Join(root, "missing")
	testutil.Equal(t, run(context.Background(), ws).Status, gate.StatusFail)
}

func TestShardRunKeepsOnlyItsMutants(t *testing.T) {
	ws, fg := setup(t, func(mutated string, _ []string) outcome {
		return outcome{killed: !strings.Contains(mutated, "a >= 0")}
	})
	ws.Mutation = gate.MutationPlan{Shards: 4, Shard: 1}
	out := Check(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values[ValueShard], float64(1))
	testutil.Equal(t, out.Values[ValueMutants], float64(2))
	s, err := readShard(ws.Report(ShardFile))
	testutil.NoErr(t, err)
	testutil.Equal(t, s.Shard, 1)
	testutil.Equal(t, s.Shards, 4)
	testutil.Equal(t, s.Total, 6)
	testutil.Equal(t, len(s.Entries), 2)
	testutil.Equal(t, s.Entries[0].ID, 1)
	testutil.Equal(t, s.Entries[1].ID, 5)
	testutil.Missing(t, ws.Report(ReportFile))
	testutil.True(t, len(fg.overlays) <= 2, "only the shard's mutants run")
	testutil.Equal(t, ShardDir(3), "mutation-shard-3")
}

func TestShardReportThatCannotBeKeptFailsTheCheck(t *testing.T) {
	ws := gate.Workspace{Out: filepath.Join(t.TempDir(), "missing"), Mutation: gate.MutationPlan{Shards: 2, Shard: 0}}
	out := Judge(ws, Report{Counts: map[Status]int{}}, nil)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Contains(t, out.Reason, "cannot keep the mutation shard report")
}

func TestPickedSplitsMutantsByRemainder(t *testing.T) {
	testutil.Equal(t, picked(gate.MutationPlan{}, 3), []int{0, 1, 2})
	testutil.Equal(t, picked(gate.MutationPlan{Shards: 3, Shard: 2}, 7), []int{2, 5})
	testutil.Equal(t, picked(gate.MutationPlan{Shards: 3, Shard: 0, Merge: "x"}, 4), []int{0, 1, 2, 3})
}
