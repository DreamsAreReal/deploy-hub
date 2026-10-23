package runner

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const otherApp = "api"

type fakeHost struct {
	calls []string
	err   error
}

func (h *fakeHost) Sync(_ context.Context, app, tag string) error {
	h.calls = append(h.calls, app+" "+tag)
	return h.err
}

func withHost(e *env, err error) *fakeHost {
	h := &fakeHost{err: err}
	e.svc.Host, e.svc.HostApps = h, []string{otherApp, appName}
	return h
}

func TestHostFilesFollowAHealthyDeploy(t *testing.T) {
	e := newEnv(t)
	h := withHost(e, nil)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, slices.Equal(h.calls, []string{appName + " " + goodTag}), true)
	e.expectJournal("web@aaaaaaa deploy ok")
	e.expectLog("host sync done")
}

func TestAppsWithoutAHostSpecAreNotSynced(t *testing.T) {
	e := newEnv(t)
	h := withHost(e, nil)
	e.svc.HostApps = []string{otherApp}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, len(h.calls), 0)
}

func TestBrokenReleaseNeverReachesTheHost(t *testing.T) {
	e := newEnv(t)
	h := withHost(e, nil)
	testutil.Equal(t, e.dispatch("deploy web "+badTag, meta()), ExitFail)
	testutil.Equal(t, len(h.calls), 0)
}

func TestHostSyncFailureRollsTheContainersBack(t *testing.T) {
	e := newEnv(t)
	withHost(e, errSim)
	e.state(registry.DeployState{Current: goodTag})
	testutil.Equal(t, e.dispatch("deploy web "+good2Tag, meta()), ExitFail)
	e.expectJournal("web@bbbbbbb deploy fail", "web@aaaaaaa rollback ok")
	e.expectOut(e.err, "error: host files not installed, previous ones kept: simulated failure", "rolled back to previous sha (aaaaaaa)")
	testutil.Equal(t, e.sim.images[mainName], image+":"+goodTag)
	testutil.Equal(t, registry.LoadState(e.appDir).Current, goodTag)
	e.expectLog("deploy failed host sync")
}

func TestHostSyncFailureOnFirstDeployStopsTheApp(t *testing.T) {
	e := newEnv(t)
	withHost(e, errSim)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitFail)
	e.expectJournal("web@aaaaaaa deploy fail", "web@aaaaaaa stop ok")
	testutil.Equal(t, e.sim.running[mainName], false)
	testutil.Equal(t, registry.LoadState(e.appDir).Current, "")
}

func TestRollbackReinstallsTheTargetsHostFiles(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{nil, ExitOK}, {errSim, ExitFail}} {
		e := newEnv(t)
		h := withHost(e, tc.err)
		e.sim.startOld(good2Tag)
		e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
		testutil.Equal(t, e.dispatch("rollback web", ""), tc.code)
		testutil.Equal(t, slices.Equal(h.calls, []string{appName + " " + goodTag}), true)
		e.expectJournal("web@aaaaaaa rollback ok")
	}
}

func TestSudoHostRunsOnlyTheHostSyncCommand(t *testing.T) {
	f := &procexectest.Fake{}
	h := SudoHost{Exec: f, Binary: "/opt/deploy-hub/bin/deploy-hub"}
	testutil.NoErr(t, h.Sync(context.Background(), appName, goodTag))
	testutil.Equal(t, f.Lines()[0], "sudo -n /opt/deploy-hub/bin/deploy-hub host-sync web "+goodTag)
	failing := SudoHost{Exec: &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stderr: "not gated"}, errors.New("exit status 1")
	}}, Binary: "x"}
	err := failing.Sync(context.Background(), appName, goodTag)
	testutil.Fails(t, err)
	testutil.Contains(t, err.Error(), "exit status 1: not gated")
}
