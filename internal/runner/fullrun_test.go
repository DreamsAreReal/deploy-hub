package runner

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fullMeta = "mode=full\n"
	planLine = "plan="
)

var night = time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)

func (e *env) at(t time.Time) {
	e.svc.Now = func() time.Time { return t }
}

func runMeta(t *testing.T, run mutation.RunSummary) string {
	t.Helper()
	data, err := json.Marshal(run)
	testutil.NoErr(t, err)
	return "run=" + base64.StdEncoding.EncodeToString(data) + "\n"
}

func (e *env) plan() string {
	e.t.Helper()
	e.out.Reset()
	testutil.Equal(e.t, e.dispatch(getCmd, fullMeta), ExitOK)
	for _, line := range strings.Split(e.out.String(), "\n") {
		if p, ok := strings.CutPrefix(line, planLine); ok {
			return p
		}
	}
	return ""
}

func (e *env) cacheState() cacheState {
	e.t.Helper()
	st, err := e.svc.loadState(appName)
	testutil.NoErr(e.t, err)
	return st
}

func TestFullRunSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	e.at(night.Add(10 * time.Hour))
	testutil.Equal(t, e.plan(), planNew)
	testutil.Equal(t, e.plan(), planContinue)
	chunk := verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	testutil.Equal(t, e.dispatch(putCmd, fullMeta+chunk+runMeta(t, mutation.RunSummary{Fresh: true, Bases: []string{cacheBase}})), ExitOK)
	e.plan()
	testutil.Contains(t, e.out.String(), "plan=continue\n", "verdicts=")
	testutil.Equal(t, e.cacheState().Last, (*gate.FullRun)(nil))
	done := mutation.RunSummary{Complete: true, Passed: true, Fresh: true, Bases: []string{cacheBase}}
	testutil.Equal(t, e.dispatch(putCmd, fullMeta+verdictsMeta(t, map[string]mutation.Status{cacheKeyB: mutation.StatusKilled})+runMeta(t, done)), ExitOK)
	testutil.Contains(t, e.out.String(), "full mutation run recorded: passed=true")
	st := e.cacheState()
	testutil.Equal(t, *st.Last, gate.FullRun{Commit: strings.TrimPrefix(goodTag, "sha-"), Bases: []string{cacheBase}, Passed: true, Time: night.Add(10 * time.Hour)})
	testutil.True(t, st.SessionStarted == nil, "the session is closed")
	_, _, serr := e.svc.loadStored(appName, sessionFile)
	testutil.True(t, serr != nil, "the session verdicts are dropped")
	testutil.Equal(t, e.plan(), planSkip)
	e.out.Reset()
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitOK)
	testutil.Contains(t, e.out.String(), "last=", "verdicts=")
	e.expectLog("full mutation run recorded")
}

func TestFullRunPlan(t *testing.T) {
	passed := &gate.FullRun{Bases: []string{cacheBase}, Passed: true, Time: night.Add(-21 * time.Hour)}
	started := night.Add(-time.Hour)
	twoDays := night.Add(-48 * time.Hour)
	expired := night.Add(-SessionMaxAge)
	cases := []struct {
		name string
		st   cacheState
		now  time.Time
		want string
	}{
		{"nothing recorded", cacheState{}, night, planNew},
		{"requested after a change", cacheState{Requested: true, Last: passed}, night.Add(10 * time.Hour), planNew},
		{"nightly and due", cacheState{Last: passed}, night, planNew},
		{"due but daytime", cacheState{Last: passed}, night.Add(10 * time.Hour), planSkip},
		{"night but recent", cacheState{Last: &gate.FullRun{Passed: true, Time: night.Add(-time.Hour)}}, night, planSkip},
		{"session open", cacheState{Last: passed, SessionStarted: &started}, night, planContinue},
		{"session open for two days", cacheState{Last: passed, SessionStarted: &twoDays}, night, planContinue},
		{"session expired", cacheState{Last: passed, SessionStarted: &expired}, night.Add(10 * time.Hour), planSkip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.at(c.now)
			st := c.st
			testutil.Equal(t, e.svc.fullPlan(&st), c.want)
		})
	}
}

func TestIncrementalRunsRecordOrRequestFullRuns(t *testing.T) {
	e := newEnv(t)
	e.at(night)
	verdicts := verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	stale := mutation.RunSummary{Complete: false, Stale: true, Bases: []string{cacheBase}}
	testutil.Equal(t, e.dispatch(putCmd, verdicts+runMeta(t, stale)), ExitOK)
	testutil.True(t, e.cacheState().Requested && e.cacheState().Last == nil, "a stale incremental run requests a full run")
	e.expectLog("full mutation run requested")
	fresh := mutation.RunSummary{Complete: true, Passed: false, Fresh: true, Stale: true, Bases: []string{cacheBase}}
	testutil.Equal(t, e.dispatch(putCmd, verdicts+runMeta(t, fresh)), ExitOK)
	st := e.cacheState()
	testutil.True(t, !st.Requested && st.Last != nil && !st.Last.Passed, "a complete fresh run is a full run, failed ones too")
	cached := mutation.RunSummary{Complete: true, Passed: true, Fresh: false, Bases: []string{cacheBase}}
	testutil.Equal(t, e.dispatch(putCmd, verdicts+runMeta(t, cached)), ExitOK)
	testutil.True(t, !e.cacheState().Last.Passed, "a run with cached verdicts is not a full run")
	testutil.Equal(t, e.dispatch(putCmd, fullMeta+verdicts+runMeta(t, mutation.RunSummary{Complete: true, Passed: true, Fresh: true, Bases: []string{cacheBase}})), ExitOK)
	testutil.True(t, !e.cacheState().Last.Passed, "a full chunk without an open session is not a full run")
}

func TestMalformedRunSummaryIsRefused(t *testing.T) {
	verdicts := verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	for _, run := range []string{"run=%%%\n", "run=" + base64.StdEncoding.EncodeToString([]byte("{")) + "\n", runMeta(t, mutation.RunSummary{Bases: []string{"x"}})} {
		e := newEnv(t)
		testutil.Equal(t, e.dispatch(putCmd, verdicts+run), ExitFail)
		e.expectOut(e.err, msgBadCache)
	}
}

func TestCorruptStateStopsTheCache(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.plan(), planNew)
	path := filepath.Join(e.svc.Paths.MutationCache(appName), stateFile)
	testutil.Write(t, path, testutil.Read(t, path)+" ")
	testutil.Equal(t, e.dispatch(getCmd, fullMeta), ExitFail)
	e.expectOut(e.err, msgCacheCorrupt)
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})+runMeta(t, mutation.RunSummary{Stale: true, Bases: []string{cacheBase}})), ExitOK)
	testutil.True(t, e.cacheState().Requested, "a put rewrites a corrupt state")
}

func TestGateOfAnotherCommitRequestsFullRunAfterFailedOne(t *testing.T) {
	verdicts := verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	gateRun := runMeta(t, mutation.RunSummary{Complete: true, Passed: true, Bases: []string{cacheBase}})
	started := night.Add(-time.Hour)
	otherCommit := strings.TrimPrefix(good2Tag, "sha-")
	failedElsewhere := &gate.FullRun{Commit: otherCommit, Bases: []string{cacheBase}, Time: night.Add(-time.Hour)}
	failedHere := &gate.FullRun{Commit: strings.TrimPrefix(goodTag, "sha-"), Bases: []string{cacheBase}, Time: night.Add(-time.Hour)}
	passedElsewhere := &gate.FullRun{Commit: otherCommit, Bases: []string{cacheBase}, Passed: true, Time: night.Add(-time.Hour)}
	cases := []struct {
		name string
		st   cacheState
		meta string
		want bool
	}{
		{"failed on another commit", cacheState{Last: failedElsewhere}, "", true},
		{"failed on this commit", cacheState{Last: failedHere}, "", false},
		{"passed on another commit", cacheState{Last: passedElsewhere}, "", false},
		{"session already open", cacheState{Last: failedElsewhere, SessionStarted: &started}, "", false},
		{"full chunk without a session", cacheState{Last: failedElsewhere}, fullMeta, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.at(night.Add(10 * time.Hour))
			testutil.NoErr(t, e.svc.saveState(appName, c.st))
			testutil.Equal(t, e.dispatch(putCmd, c.meta+verdicts+gateRun), ExitOK)
			testutil.Equal(t, e.cacheState().Requested, c.want)
		})
	}
	e := newEnv(t)
	e.at(night.Add(10 * time.Hour))
	testutil.NoErr(t, e.svc.saveState(appName, cacheState{Last: failedElsewhere}))
	testutil.Equal(t, e.dispatch(putCmd, verdicts+gateRun), ExitOK)
	testutil.Equal(t, e.plan(), planNew)
}
