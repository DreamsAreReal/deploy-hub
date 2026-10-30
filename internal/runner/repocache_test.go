package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	siblingApp  = "stt"
	foreignApp  = "other"
	foreignRepo = "Owner/other"
	lastLine    = "last="
	dotDotRepo  = "Owner/.."
	tasksCache  = "tasks"
)

func (e *env) addApp(name, repo string) {
	e.t.Helper()
	dir := filepath.Join(filepath.Dir(e.appDir), name)
	testutil.NoErr(e.t, os.MkdirAll(dir, 0o750))
	list := filepath.Join(e.hubDir, "apps.list")
	e.write(list, testutil.Read(e.t, list)+name+" "+dir+"\n")
	testutil.NoErr(e.t, registry.AppConf{Repo: repo}.Save(dir))
}

func (e *env) lastFullRun(app string) *gate.FullRun {
	e.t.Helper()
	e.out.Reset()
	testutil.Equal(e.t, e.dispatch(VerbCacheGet+" "+app+" "+good2Tag, ""), ExitOK)
	for _, line := range strings.Split(e.out.String(), "\n") {
		if encoded, ok := strings.CutPrefix(line, lastLine); ok {
			data, err := base64.StdEncoding.DecodeString(encoded)
			testutil.NoErr(e.t, err)
			var last gate.FullRun
			testutil.NoErr(e.t, json.Unmarshal(data, &last))
			return &last
		}
	}
	return nil
}

func TestAppsOfOneRepositoryShareTheFullRun(t *testing.T) {
	e := newEnv(t)
	e.at(night)
	e.addApp(siblingApp, appRepo)
	e.addApp(foreignApp, foreignRepo)
	ctx := context.Background()
	job, _, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.Equal(t, job.Plan, planNew)
	sibling := VerbCachePut + " " + siblingApp + " " + goodTag
	testutil.Equal(t, e.dispatch(sibling, fullMeta+verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})+runMeta(t, mutation.RunSummary{Fresh: true, Bases: []string{cacheBase}})), ExitOK)
	job, session, err := e.svc.DispatchFull(ctx, appName)
	testutil.NoErr(t, err)
	testutil.Equal(t, job.Plan, planContinue)
	got, err := mutation.DecodeVerdicts(session)
	testutil.NoErr(t, err)
	testutil.Equal(t, got.Entries, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})
	v, r := chunk(t, map[string]mutation.Status{cacheKeyB: mutation.StatusKilled}, mutation.RunSummary{Complete: true, Passed: true, Fresh: true, Bases: []string{cacheBase}})
	testutil.NoErr(t, e.svc.ImportFull(ctx, appName, chunkCommit, v, r))
	want := &gate.FullRun{Commit: chunkCommit, Bases: []string{cacheBase}, Passed: true, Time: night}
	testutil.Equal(t, e.lastFullRun(siblingApp), want)
	testutil.Equal(t, e.lastFullRun(appName), want)
	testutil.Equal(t, e.served().Entries, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled, cacheKeyB: mutation.StatusKilled})
	testutil.Equal(t, e.lastFullRun(foreignApp), (*gate.FullRun)(nil))
	_, err = os.Stat(e.svc.Paths.MutationCache(siblingApp))
	testutil.True(t, os.IsNotExist(err), "the second app of a repository keeps no cache of its own")
}

func TestGateOfAnotherAppOfTheRepositoryRequestsItsFullRun(t *testing.T) {
	e := newEnv(t)
	e.at(night.Add(10 * time.Hour))
	e.addApp(siblingApp, appRepo)
	failed := &gate.FullRun{Commit: chunkCommit, Bases: []string{cacheBase}, Time: night}
	testutil.NoErr(t, e.svc.saveState(appName, cacheState{Last: failed}))
	gateRun := runMeta(t, mutation.RunSummary{Complete: true, Passed: true, Bases: []string{cacheBase}})
	testutil.Equal(t, e.dispatch(VerbCachePut+" "+siblingApp+" "+goodTag, verdictsMeta(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled})+gateRun), ExitOK)
	testutil.True(t, e.cacheState().Requested, "the gate of the second app requests the repository's full run")
	job, _, err := e.svc.DispatchFull(context.Background(), appName)
	testutil.NoErr(t, err)
	testutil.Equal(t, job.Plan, planNew)
}

func TestCacheOfARepositoryIsNamedAfterIt(t *testing.T) {
	for repo, want := range map[string]string{"Owner/Tasks": tasksCache, appRepo: appName} {
		got, err := repoCache(registry.AppConf{Repo: repo})
		testutil.NoErr(t, err)
		testutil.Equal(t, got, want)
	}
	for _, repo := range []string{"", dotDotRepo, "Owner/a/b", "Owner/.hidden", tasksCache} {
		_, err := repoCache(registry.AppConf{Repo: repo})
		testutil.True(t, err != nil, "a repository that cannot name a cache directory is refused: "+repo)
	}
}

func TestAppWithoutAUsableRepositoryHasNoCache(t *testing.T) {
	e := newEnv(t)
	e.conf(registry.AppConf{Repo: dotDotRepo})
	testutil.Equal(t, e.dispatch(getCmd, ""), ExitFail)
	e.expectOut(e.err, msgNoRepo)
	ctx := context.Background()
	_, _, err := e.svc.DispatchFull(ctx, appName)
	testutil.True(t, err != nil, "no full run for an app without a usable repository")
	v, r := chunk(t, map[string]mutation.Status{cacheKeyA: mutation.StatusKilled}, mutation.RunSummary{Fresh: true, Bases: []string{cacheBase}})
	testutil.True(t, e.svc.ImportFull(ctx, appName, chunkCommit, v, r) != nil, "no import for an app without a usable repository")
	_, err = os.Stat(filepath.Join(e.hubDir, "mutation-cache"))
	testutil.True(t, os.IsNotExist(err), "nothing is stored")
}
