package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func hostOnlyEnv(t *testing.T, syncErr error) (*env, *fakeHost) {
	t.Helper()
	e := newEnv(t)
	h := withHost(e, syncErr)
	e.svc.HostOnly = []string{appName}
	e.conf(registry.AppConf{Profile: registry.ProfileHost, Image: image, Repo: appRepo})
	return e, h
}

func (e *env) noContainerTouched() {
	e.t.Helper()
	for _, verb := range []string{"docker compose", "docker run", "docker stop", "docker rm"} {
		testutil.Equal(e.t, e.exec.Count(verb), 0)
	}
	testutil.Equal(e.t, len(e.sim.running), 0)
}

func TestHostOnlyDeployInstallsTheFilesWithoutAContainer(t *testing.T) {
	e, h := hostOnlyEnv(t, nil)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, identity()), ExitOK)
	testutil.Equal(t, slices.Equal(h.calls, []string{appName + " " + goodTag}), true)
	testutil.Equal(t, e.exec.Count("docker pull -q "+image+"@"+digest), 1)
	e.noContainerTouched()
	e.expectJournal("web@aaaaaaa deploy ok")
	e.expectOut(e.out, "host files: ok — web@aaaaaaa live")
	testutil.Equal(t, registry.LoadState(e.appDir), registry.DeployState{Current: goodTag})
}

func TestHostOnlyDeployFailureKeepsThePreviousVersion(t *testing.T) {
	e, _ := hostOnlyEnv(t, errSim)
	e.state(registry.DeployState{Current: goodTag})
	testutil.Equal(t, e.dispatch("deploy web "+good2Tag, identity()), ExitFail)
	e.noContainerTouched()
	e.expectJournal("web@bbbbbbb deploy fail")
	e.expectOut(e.err, "error: host files not installed, previous ones kept: simulated failure")
	e.expectLog("deploy failed host sync")
	testutil.Equal(t, registry.LoadState(e.appDir).Current, goodTag)
	testutil.True(t, !strings.Contains(e.journal(), "rollback"), "host-sync restores the files itself, nothing to roll back")
}

func TestHostOnlyAppWithoutAHostSpecIsRefused(t *testing.T) {
	e, h := hostOnlyEnv(t, nil)
	e.svc.HostApps = []string{otherApp}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, identity()), ExitFail)
	testutil.Equal(t, len(h.calls), 0)
	e.expectOut(e.err, "error: web ships only host files, but this hub has no host spec for it")
	e.expectJournal("web@aaaaaaa deploy fail")
	e.noContainerTouched()
	e2, _ := hostOnlyEnv(t, nil)
	e2.svc.Host = nil
	testutil.Equal(t, e2.dispatch("deploy web "+goodTag, identity()), ExitFail)
}

func TestHostOnlyRollbackReinstallsTheEarlierFiles(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  int
		entry string
		cur   string
	}{{nil, ExitOK, "web@aaaaaaa rollback ok", goodTag}, {errSim, ExitFail, "web@aaaaaaa rollback fail", good2Tag}} {
		e, h := hostOnlyEnv(t, tc.err)
		e.deployedBefore(goodTag, good2Tag)
		e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
		testutil.Equal(t, e.dispatch("rollback web", ""), tc.code)
		testutil.Equal(t, slices.Equal(h.calls, []string{appName + " " + goodTag}), true)
		e.noContainerTouched()
		e.expectJournal(tc.entry)
		testutil.Equal(t, registry.LoadState(e.appDir).Current, tc.cur)
	}
}

func TestHostOnlyStatusFollowsTheLastHostSync(t *testing.T) {
	for _, tc := range []struct {
		journal string
		line    string
		up      float64
	}{
		{"", "web | aaaaaaa | missing/- | never", 0},
		{"[2026-10-19T11:00:00Z] web@aaaaaaa host-sync ok 3s changed=2\n[2026-10-19T11:00:01Z] web@aaaaaaa deploy ok 5s\n", "web | aaaaaaa | installed/healthy | 2026-10-19T11:00:01Z", 1},
		{"[2026-10-19T11:00:00Z] web@aaaaaaa host-sync ok 3s changed=2\n[2026-10-19T11:05:00Z] web@bbbbbbb host-sync fail 1s changed=0\n", "web | aaaaaaa | not-installed/unhealthy | never", 0},
	} {
		e, _ := hostOnlyEnv(t, nil)
		e.state(registry.DeployState{Current: goodTag})
		e.write(filepath.Join(e.hubDir, "deploys.log"), tc.journal)
		testutil.Equal(t, e.dispatch("status", ""), ExitOK)
		e.expectOut(e.out, tc.line)
		testutil.Equal(t, metricFor(e, "deploy_hub_app_up", appName), tc.up)
		testutil.Equal(t, metricFor(e, "deploy_hub_app_healthy", appName), tc.up)
		testutil.Equal(t, e.exec.Count("docker"), 0)
	}
}
