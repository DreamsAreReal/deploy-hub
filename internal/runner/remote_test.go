package runner

import (
	"context"
	"slices"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type fakeRemote struct {
	calls []string
	err   error
}

func (r *fakeRemote) Run(_ context.Context, app, tag string) error {
	r.calls = append(r.calls, app+" "+tag)
	return r.err
}

func withRemote(e *env, err error) *fakeRemote {
	r := &fakeRemote{err: err}
	e.svc.Remote, e.svc.RemoteApps = r, []string{otherApp, appName}
	return r
}

func TestRemoteFilesFollowAHealthyDeploy(t *testing.T) {
	e := newEnv(t)
	r := withRemote(e, nil)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, slices.Equal(r.calls, []string{appName + " " + goodTag}), true)
	e.expectLog("remote sync done")
}

func TestAnUnreachableTargetDoesNotFailTheDeploy(t *testing.T) {
	e := newEnv(t)
	withRemote(e, errSim)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	e.expectJournal("web@aaaaaaa deploy ok")
	e.expectOut(e.err, "warning: remote files not delivered, the target keeps its version: simulated failure")
	testutil.Equal(t, registry.LoadState(e.appDir).Current, goodTag)
}

func TestOnlyHealthyDeploysOfRemoteAppsReachTheTarget(t *testing.T) {
	e := newEnv(t)
	r := withRemote(e, nil)
	testutil.Equal(t, e.dispatch("deploy web "+badTag, meta()), ExitFail)
	e.svc.RemoteApps = []string{otherApp}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, len(r.calls), 0)
}

func TestARollbackSendsTheTargetsRemoteFiles(t *testing.T) {
	e := newEnv(t)
	r := withRemote(e, nil)
	e.sim.startOld(good2Tag)
	e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
	testutil.Equal(t, e.dispatch("rollback web", ""), ExitOK)
	testutil.Equal(t, slices.Equal(r.calls, []string{appName + " " + goodTag}), true)
}
