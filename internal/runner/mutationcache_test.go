package runner

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var (
	cacheKeyA = strings.Repeat("a", 64)
	cacheKeyB = strings.Repeat("b", 64)
	cacheBase = strings.Repeat("c", 64)
)

const (
	putCmd = VerbCachePut + " " + appName + " " + goodTag
	getCmd = VerbCacheGet + " " + appName + " " + goodTag
)

func verdictsMeta(t *testing.T, entries map[string]mutation.Status) string {
	t.Helper()
	data, err := mutation.Verdicts{Version: 1, Bases: []string{cacheBase}, Entries: entries}.Encode()
	testutil.NoErr(t, err)
	return "verdicts=" + base64.StdEncoding.EncodeToString(data) + "\n"
}

func (e *env) served() mutation.Verdicts {
	e.t.Helper()
	var sum, encoded string
	for _, line := range strings.Split(e.out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "sha256="); ok {
			sum = v
		}
		if v, ok := strings.CutPrefix(line, "verdicts="); ok {
			encoded = v
		}
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	testutil.NoErr(e.t, err)
	testutil.Equal(e.t, mutation.Digest(data), sum)
	v, err := mutation.DecodeVerdicts(data)
	testutil.NoErr(e.t, err)
	return v
}

func TestMutationCacheRoundTripAndMerge(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitOK)
	testutil.Equal(t, e.out.String(), "")
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})), ExitOK)
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyB: mutation.StatusSurvived})), ExitOK)
	testutil.Contains(t, e.out.String(), "mutation cache: 2 verdicts, sha256 ")
	testutil.True(t, e.locker.released, "the cache lock is released")
	e.out.Reset()
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitOK)
	v := e.served()
	testutil.Equal(t, v.Entries, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled, cacheKeyB: mutation.StatusSurvived})
	want := e.ident.wants[len(e.ident.wants)-1]
	testutil.Equal(t, []string{want.Repository, want.WorkflowRef, want.Commit}, []string{appRepo, hubRef, strings.TrimPrefix(goodTag, "sha-")})
	e.expectLog("mutation cache served")
}

func TestMutationCacheNeedsTheHubWorkflowIdentity(t *testing.T) {
	for _, raw := range []string{getCmd, putCmd} {
		e := newEnv(t)
		e.bare = true
		testutil.Equal(t, e.dispatch(raw, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})), ExitFail)
		e.expectOut(e.err, msgNoIdentity)
		e2 := newEnv(t)
		e2.ident.err = oidc.ErrClaims
		testutil.Equal(t, e2.dispatch(raw, ""), ExitFail)
		e2.expectOut(e2.err, msgUnverified)
		_, err := os.Stat(e2.svc.Paths.MutationCache(appName))
		testutil.True(t, errors.Is(err, os.ErrNotExist), "nothing is stored for a refused request")
	}
	e := newEnv(t)
	testutil.NoErr(t, os.Remove(filepath.Join(e.appDir, "app.conf")))
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitFail)
}

func TestMutationCachePutRejectsMalformedVerdicts(t *testing.T) {
	cases := map[string]string{
		"missing payload":    "",
		"payload not base64": "verdicts=%%%\n",
		"payload not json":   "verdicts=" + base64.StdEncoding.EncodeToString([]byte("{")) + "\n",
		"bad key":            "verdicts=" + base64.StdEncoding.EncodeToString([]byte(`{"version":1,"entries":{"k":"killed"}}`)) + "\n",
		"not a final":        "verdicts=" + base64.StdEncoding.EncodeToString([]byte(`{"version":1,"entries":{"`+strings.Repeat("a", 64)+`":"errored"}}`)) + "\n",
	}
	for name, stdin := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			testutil.Equal(t, e.dispatch(putCmd, stdin), ExitFail)
			e.expectOut(e.err, msgBadCache)
			e.expectLog("mutation verdicts rejected")
			e.expectJournal("refuse denied(" + Fingerprint(putCmd))
		})
	}
}

func TestTamperedStoredCacheIsNotServed(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusSurvived})), ExitOK)
	path := filepath.Join(e.svc.Paths.MutationCache(appName), cacheFile)
	testutil.Write(t, path, strings.Replace(testutil.Read(t, path), string(mutation.StatusSurvived), string(mutation.StatusKilled), 1))
	e.out.Reset()
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitFail)
	e.expectOut(e.err, msgCacheCorrupt)
	testutil.Equal(t, e.out.String(), "")
	e.expectLog("stored mutation cache does not match its sha256")
	testutil.True(t, e.svc.samples[len(e.svc.samples)-1].Name == obs.MetricMutationCacheIntact, "intact metric recorded")
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyB: mutation.StatusKilled})), ExitOK)
	e.expectLog("stored mutation cache replaced")
	e.out.Reset()
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitOK)
	v := e.served()
	testutil.Equal(t, v.Entries, map[string]mutation.Status{cacheKeyB: mutation.StatusKilled})
}

func TestMutationCachePutFailures(t *testing.T) {
	e := newEnv(t)
	e.locker.err = errors.New("busy")
	testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})), ExitFail)
	e.expectOut(e.err, msgCacheBusy)
	e2 := newEnv(t)
	testutil.Write(t, filepath.Join(e2.hubDir, "mutation-cache"), "a file where the directory belongs")
	testutil.Equal(t, e2.dispatch(putCmd, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})), ExitFail)
	e2.expectOut(e2.err, msgCacheWrite)
}

func TestOversizedCacheKeepsOnlyTheLatestRun(t *testing.T) {
	e := newEnv(t)
	half := MaxCacheEntries * 3 / 5
	for run := range 2 {
		entries := make(map[string]mutation.Status, half)
		for i := range half {
			entries[mutation.Digest([]byte{byte(run), byte(i), byte(i >> 8), byte(i >> 16)})] = mutation.StatusKilled
		}
		testutil.Equal(t, e.dispatch(putCmd, verdictsMeta(t, entries)), ExitOK)
	}
	testutil.Contains(t, e.out.String(), "mutation cache: "+strconv.Itoa(half)+" verdicts, sha256", "mutation cache: "+strconv.Itoa(half)+" verdicts, sha256")
	e.expectLog("mutation cache over its size limit, keeping only the latest run")
}
