package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const chunkCommit = "0123456789abcdef0123456789abcdef01234567"

func chunk(t *testing.T, entries map[string]mutation.Status, run mutation.RunSummary) ([]byte, []byte) {
	t.Helper()
	v, err := mutation.Verdicts{Version: 1, Bases: []string{cacheBase}, Entries: entries}.Encode()
	testutil.NoErr(t, err)
	r, err := json.Marshal(run)
	testutil.NoErr(t, err)
	return v, r
}

func TestServerFullRunSession(t *testing.T) {
	e := newEnv(t)
	e.at(night)
	ctx := context.Background()
	job, session, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.Equal(t, job, FullJob{Plan: planNew, App: appName, Repo: appRepo})
	testutil.True(t, job.Runs() && session == nil, "a new session starts empty")
	v, r := chunk(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled}, mutation.RunSummary{Fresh: true, Bases: []string{cacheBase}})
	testutil.NoErr(t, e.svc.ImportFull(ctx, appName, chunkCommit, v, r))
	job, session, err = e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.Equal(t, job.Plan, planContinue)
	testutil.Equal(t, job.SessionSHA256, mutation.Digest(session))
	got, err := mutation.DecodeVerdicts(session)
	testutil.NoErr(t, err)
	testutil.Equal(t, got.Entries, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	v, r = chunk(t, map[string]mutation.Status{cacheKeyB: mutation.StatusSurvived}, mutation.RunSummary{Complete: true, Passed: true, Fresh: true, Bases: []string{cacheBase}})
	testutil.NoErr(t, e.svc.ImportFull(ctx, appName, chunkCommit, v, r))
	st := e.cacheState()
	testutil.True(t, st.Last != nil && st.Last.Passed && st.Last.Commit == chunkCommit && st.SessionStarted == nil, "the completing chunk records the full run")
	job, _, err = e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.True(t, job.Plan == planSkip && !job.Runs(), "nothing to run after a fresh full run")
}

func TestServerContinueWithoutVerdictsYet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	job, session, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.True(t, job.Plan == planContinue && session == nil && job.SessionSHA256 == "", "an open session without verdicts serves none")
}

func TestServerImportRejectsForgedChunks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, r := chunk(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled}, mutation.RunSummary{Bases: []string{cacheBase}})
	cases := map[string][3]string{
		"bad commit":      {"abc", string(v), string(r)},
		"bad verdicts":    {chunkCommit, `{"version":1,"entries":{"k":"killed"}}`, string(r)},
		"bad run summary": {chunkCommit, string(v), "{"},
		"bad base":        {chunkCommit, string(v), `{"bases":["x"]}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := e.svc.ImportFull(ctx, appName, c[0], []byte(c[1]), []byte(c[2]))
			testutil.True(t, errors.Is(err, ErrImport), "rejected: "+name)
		})
	}
	_, err := os.Stat(e.svc.Paths.MutationCache(appName))
	testutil.True(t, errors.Is(err, os.ErrNotExist), "nothing is stored from a forged chunk")
}

func TestServerDispatchAndImportFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _, err := e.svc.DispatchFull(ctx, "ghost")
	testutil.Fails(t, err)
	e.locker.err = errors.New("busy")
	_, _, err = e.svc.DispatchFull(ctx, appName)
	testutil.Fails(t, err)
	v, r := chunk(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled}, mutation.RunSummary{Bases: []string{cacheBase}})
	testutil.Fails(t, e.svc.ImportFull(ctx, appName, chunkCommit, v, r))
	e.locker.err = nil
	testutil.Write(t, filepath.Join(e.hubDir, "mutation-cache"), "a file where the directory belongs")
	testutil.True(t, errors.Is(e.svc.ImportFull(ctx, appName, chunkCommit, v, r), ErrImport), "a store failure is reported")
	testutil.NoErr(t, os.Remove(filepath.Join(e.appDir, registry.AppConfFile)))
	_, _, err = e.svc.DispatchFull(ctx, appName)
	testutil.Fails(t, err)
}

func TestServerDispatchStopsOnACorruptState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	path := filepath.Join(e.svc.Paths.MutationCache(appName), stateFile)
	testutil.Write(t, path, testutil.Read(t, path)+" ")
	_, _, err = e.svc.DispatchFull(ctx, appName)
	testutil.True(t, errors.Is(err, errCacheCorrupt), "a tampered state stops the dispatch")
}
