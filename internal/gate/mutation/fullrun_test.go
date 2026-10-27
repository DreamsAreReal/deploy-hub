package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const lastCommit = "abc1234"

func TestCachedVerdictsNeedAPassedFullRunOfTheSameBase(t *testing.T) {
	ws, fg := cachedSetup(t)
	full, _ := runOnce(t, ws, fg)
	testutil.Equal(t, Judge(ws, full, nil).Status, gate.StatusPass)
	ws.Mutation = keepVerdicts(t, ws)
	cached, _ := runOnce(t, ws, fg)
	cases := []struct {
		name   string
		last   *gate.FullRun
		full   bool
		reason string
	}{
		{"never ran in full", nil, false, reasonNoFull},
		{"last full run failed", &gate.FullRun{Commit: lastCommit, Bases: []string{cached.Base}}, false, "(" + lastCommit + ") is below"},
		{"base changed since", &gate.FullRun{Commit: lastCommit, Bases: []string{baseOne}, Passed: true}, false, reasonStale},
		{"passed full run of this base", &gate.FullRun{Commit: lastCommit, Bases: []string{cached.Base}, Passed: true}, false, ""},
		{"a full run chunk itself", nil, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws.Mutation.Last, ws.Mutation.Full = c.last, c.full
			out := Judge(ws, cached, nil)
			testutil.Equal(t, out.Status == gate.StatusPass, c.reason == "")
			testutil.Contains(t, out.Reason, c.reason)
			testutil.Equal(t, out.Values[ValueCached], float64(cached.Cached))
		})
	}
}

func readRun(t *testing.T, ws gate.Workspace) ProjectRun {
	t.Helper()
	var pr ProjectRun
	testutil.NoErr(t, json.Unmarshal([]byte(testutil.Read(t, ws.Report(RunFile))), &pr))
	return pr
}

func TestEveryJudgedRunLeavesItsSummary(t *testing.T) {
	ws, fg := cachedSetup(t)
	full, _ := runOnce(t, ws, fg)
	Judge(ws, full, nil)
	testutil.Equal(t, readRun(t, ws), ProjectRun{Base: full.Base, Complete: true, Passed: true, Fresh: true, Score: full.Score()})
	ws.Mutation = keepVerdicts(t, ws)
	cached, _ := runOnce(t, ws, fg)
	Judge(ws, cached, nil)
	testutil.Equal(t, readRun(t, ws), ProjectRun{Base: cached.Base, Complete: true, Passed: false, Fresh: false, Score: cached.Score()})
	ws.Mutation.Full = true
	Judge(ws, cached, ErrIncomplete)
	testutil.Equal(t, readRun(t, ws), ProjectRun{Base: cached.Base, Complete: false, Passed: false, Fresh: true, Score: cached.Score()})
	ws.Out = filepath.Join(ws.Out, "missing")
	Judge(ws, cached, nil)
}

func TestBudgetStopsTheRunAndKeepsWhatFinished(t *testing.T) {
	ws, fg := cachedSetup(t)
	ws.Mutation.Budget = time.Nanosecond
	_, err := Run(context.Background(), ws)
	testutil.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline"), "the budget ends the run")
	testutil.Equal(t, len(fg.overlays), 0)
}

func writeRuns(t *testing.T, runs ...ProjectRun) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(runs))
	for i, pr := range runs {
		data, err := json.Marshal(pr)
		testutil.NoErr(t, err)
		p := filepath.Join(dir, string(rune('a'+i)), RunFile)
		testutil.Write(t, p, string(data))
		paths = append(paths, p)
	}
	return paths
}

func TestSummarizeRuns(t *testing.T) {
	last := &gate.FullRun{Bases: []string{baseOne}}
	done := ProjectRun{Base: baseOne, Complete: true, Passed: true, Fresh: true}
	other := ProjectRun{Base: baseTwo, Complete: true, Passed: true, Fresh: false}
	cases := []struct {
		name string
		runs []ProjectRun
		last *gate.FullRun
		want RunSummary
	}{
		{"nothing ran", nil, last, RunSummary{Fresh: true}},
		{"one fresh project", []ProjectRun{done}, last, RunSummary{Complete: true, Passed: true, Fresh: true, Bases: []string{baseOne}}},
		{"a new base", []ProjectRun{done, other}, last, RunSummary{Complete: true, Passed: true, Stale: true, Bases: []string{baseOne, baseTwo}}},
		{"no full run yet", []ProjectRun{done}, nil, RunSummary{Complete: true, Passed: true, Fresh: true, Stale: true, Bases: []string{baseOne}}},
		{"an incomplete project", []ProjectRun{done, {Complete: false, Fresh: true}}, last, RunSummary{Fresh: true, Bases: []string{baseOne}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SummarizeRuns(writeRuns(t, c.runs...), c.last)
			testutil.NoErr(t, err)
			testutil.Equal(t, got, c.want)
		})
	}
	paths := writeRuns(t, done)
	testutil.Write(t, paths[0], "{")
	_, err := SummarizeRuns(paths, last)
	testutil.True(t, err != nil, "a broken summary is an error")
}
