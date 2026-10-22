package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	ShardFile       = "mutation-shard.json"
	ShardDirPrefix  = "mutation-shard-"
	shardVerdict    = "verdict.json"
	ValueShard      = "shard"
	reasonUnsharded = "the policy runs mutation testing in %d shards; the gate needs their reports"
	shardUnreadable = "shard %d: %w"
	shardForeign    = "shard %d holds shard %d of %d over %d mutants, expected shard %d of %d over %d"
	shardStray      = "shard %d reports mutant %d of %d"
	shardShort      = "the shards report %d of %d mutants"
	shardOther      = "shard %d ran on %s %s@%s, not on %s %s@%s"
)

var ErrShards = errors.New("mutation shard reports do not add up")

type shardReport struct {
	Shard   int     `json:"shard"`
	Shards  int     `json:"shards"`
	Total   int     `json:"total"`
	Entries []entry `json:"entries"`
}

func ShardDir(shard int) string {
	return ShardDirPrefix + strconv.Itoa(shard)
}

func picked(plan gate.MutationPlan, total int) []int {
	ids := make([]int, 0, total)
	for i := range total {
		if !plan.ShardRun() || i%plan.Shards == plan.Shard {
			ids = append(ids, i)
		}
	}
	return ids
}

func Sharded(run func(context.Context, gate.Workspace) gate.Outcome) func(context.Context, gate.Workspace) gate.Outcome {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		plan := ws.Mutation
		switch {
		case !plan.Sharded() || plan.ShardRun():
			return run(ctx, ws)
		case plan.Merge == "":
			return gate.Fail(fmt.Sprintf(reasonUnsharded, plan.Shards))
		}
		rep, err := Merge(plan)
		return Judge(ws, rep, err)
	}
}

func (r Report) saveShard(ws gate.Workspace) (gate.Outcome, error) {
	data, err := json.Marshal(shardReport{Shard: ws.Mutation.Shard, Shards: ws.Mutation.Shards, Total: r.Total, Entries: r.Entries})
	if err == nil {
		err = os.WriteFile(ws.Report(ShardFile), data, filePerm)
	}
	values := map[string]float64{ValueShard: float64(ws.Mutation.Shard), ValueMutants: float64(len(r.Entries))}
	for s, n := range r.Counts {
		values[string(s)] = float64(n)
	}
	return gate.Pass(values), err
}

type merger struct {
	plan    gate.MutationPlan
	total   int
	seen    map[int]bool
	entries []entry
}

func Merge(plan gate.MutationPlan) (Report, error) {
	m := &merger{plan: plan, total: -1, seen: map[int]bool{}}
	for k := range plan.Shards {
		if err := m.take(k); err != nil {
			return Report{}, err
		}
	}
	if len(m.seen) != m.total {
		return Report{}, fmt.Errorf("%w: "+shardShort, ErrIncomplete, len(m.seen), m.total)
	}
	slices.SortFunc(m.entries, func(a, b entry) int { return a.ID - b.ID })
	rep := Report{Counts: map[Status]int{}, Total: m.total}
	for _, e := range m.entries {
		record(&rep, e)
	}
	return rep, nil
}

func (m *merger) take(k int) error {
	s, err := m.load(k)
	if err != nil {
		return err
	}
	if s.Shard != k || s.Shards != m.plan.Shards || (m.total >= 0 && s.Total != m.total) {
		return fmt.Errorf("%w: "+shardForeign, ErrShards, k, s.Shard, s.Shards, s.Total, k, m.plan.Shards, m.total)
	}
	m.total = s.Total
	for _, e := range s.Entries {
		if err := m.accept(k, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *merger) load(k int) (shardReport, error) {
	s, err := readShard(filepath.Join(m.plan.Merge, ShardDir(k), m.plan.Sub, ShardFile))
	if err == nil {
		err = sameSubject(m.plan, k)
	}
	if err != nil {
		return s, fmt.Errorf("%w: "+shardUnreadable, ErrIncomplete, k, err)
	}
	return s, nil
}

func (m *merger) accept(k int, e entry) error {
	if e.ID < 0 || e.ID >= m.total || e.ID%m.plan.Shards != k || m.seen[e.ID] {
		return fmt.Errorf("%w: "+shardStray, ErrShards, k, e.ID, m.total)
	}
	m.seen[e.ID] = true
	m.entries = append(m.entries, e)
	return nil
}

func sameSubject(plan gate.MutationPlan, k int) error {
	var v gate.Verdict
	data, err := os.ReadFile(filepath.Join(plan.Merge, ShardDir(k), shardVerdict))
	if err == nil {
		err = json.Unmarshal(data, &v)
	}
	if err == nil && (v.App != plan.App || v.Repo != plan.Repo || v.Commit != plan.Commit) {
		err = fmt.Errorf(shardOther, k, v.Repo, v.App, v.Commit, plan.Repo, plan.App, plan.Commit)
	}
	return err
}

func readShard(path string) (shardReport, error) {
	var s shardReport
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	return s, err
}
