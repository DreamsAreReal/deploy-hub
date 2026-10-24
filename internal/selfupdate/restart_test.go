package selfupdate

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	busyRepo = "stt"
	idleRepo = "tasks"
	showCmd  = procexec.CIRunnerSystemctl + " show -p ControlGroup --value ci-runner@"
	restart  = procexec.CIRunnerSystemctl + " try-restart ci-docker@"
)

func idleHost(t *testing.T, ex procexec.Runner) Runners {
	t.Helper()
	root := t.TempDir()
	return Runners{
		Pending: filepath.Join(root, "ci-runner-restart"),
		Wants:   filepath.Join(root, "wants"),
		Cgroups: filepath.Join(root, "cgroup"),
		Proc:    filepath.Join(root, "proc"),
		Exec:    ex,
		Log:     slog.New(slog.DiscardHandler),
	}
}

type ciHost struct {
	*runnerFixture
	pid int
}

func newCIHost(t *testing.T, repos ...string) *ciHost {
	t.Helper()
	h := &ciHost{runnerFixture: newRunnerFixture(t), pid: 100}
	h.script[restart] = procexectest.Reply{}
	for _, repo := range repos {
		testutil.Write(t, filepath.Join(h.r.Runners.Wants, runnerUnitPrefix+repo+unitSuffix), "")
		cg := "/user.slice/user-1001.slice/" + runnerUnitPrefix + repo + unitSuffix
		h.script[showCmd+repo] = procexectest.Reply{Res: procexec.Result{Stdout: cg + "\n"}}
		h.processes(t, repo, "", "Runner.Listener")
	}
	return h
}

func (h *ciHost) cgroup(repo string) string {
	return filepath.Join(h.r.Runners.Cgroups, "user.slice", "user-1001.slice", runnerUnitPrefix+repo+unitSuffix)
}

func (h *ciHost) processes(t *testing.T, repo, child string, names ...string) {
	t.Helper()
	pids := ""
	for _, name := range names {
		h.pid++
		pid := strconv.Itoa(h.pid)
		testutil.Write(t, filepath.Join(h.r.Runners.Proc, pid, "comm"), name+"\n")
		pids += pid + "\n"
	}
	testutil.Write(t, filepath.Join(h.cgroup(repo), child, cgroupProcs), pids)
}

func (h *ciHost) startJob(t *testing.T, repo string) {
	t.Helper()
	h.processes(t, repo, "", "runsvc.sh", "Runner.Listener", jobProcess, "go")
}

func (h *ciHost) restarts(repo string) int {
	return h.exec.Count(restart + repo + ".service ci-runner@" + repo + ".service")
}

func (h *ciHost) sync(t *testing.T) bool {
	t.Helper()
	changed, err := h.r.Sync(context.Background())
	testutil.NoErr(t, err)
	return changed
}

func TestNewRunnerFilesRestartOnlyIdleRunnersAndTheBusyOnesAfterTheirJob(t *testing.T) {
	h := newCIHost(t, busyRepo, idleRepo)
	h.installOld(t)
	h.startJob(t, busyRepo)
	testutil.True(t, h.sync(t), "new tree is installed")
	testutil.Equal(t, h.restarts(idleRepo), 1)
	testutil.Equal(t, h.restarts(busyRepo), 0)

	testutil.True(t, !h.sync(t), "tree is installed already")
	testutil.Equal(t, h.restarts(busyRepo), 0)

	h.processes(t, busyRepo, "", "Runner.Listener")
	testutil.True(t, !h.sync(t), "tree is installed already")
	testutil.Equal(t, h.restarts(busyRepo), 1)
	testutil.Equal(t, h.restarts(idleRepo), 1)

	h.sync(t)
	testutil.Equal(t, h.exec.Count(restart), 2)
	testutil.Equal(t, h.exec.Count(SetupCommand), 1)
}

func TestUnchangedRunnerFilesRestartNothing(t *testing.T) {
	h := newCIHost(t, idleRepo)
	h.sync(t)
	testutil.Equal(t, h.restarts(idleRepo), 1)
	h.sync(t)
	h.sync(t)
	testutil.Equal(t, h.restarts(idleRepo), 1)
}

func TestAJobInAChildCgroupDefersTheRestart(t *testing.T) {
	h := newCIHost(t, busyRepo)
	h.processes(t, busyRepo, "job", jobProcess)
	h.sync(t)
	testutil.Equal(t, h.restarts(busyRepo), 0)
	h.processes(t, busyRepo, "job")
	h.sync(t)
	testutil.Equal(t, h.restarts(busyRepo), 1)
}

func TestAStoppedRunnerIsRestartedOnlyIfItRuns(t *testing.T) {
	h := newCIHost(t, idleRepo)
	h.script[showCmd+idleRepo] = procexectest.Reply{Res: procexec.Result{Stdout: "\n"}}
	h.sync(t)
	testutil.Equal(t, h.restarts(idleRepo), 1)
	testutil.Missing(t, filepath.Join(h.r.Runners.Pending, idleRepo))
	g := newCIHost(t, idleRepo)
	testutil.NoErr(t, os.RemoveAll(g.cgroup(idleRepo)))
	g.sync(t)
	testutil.Equal(t, g.restarts(idleRepo), 1)
}

func TestAnUnknownRunnerStateOrAFailedRestartIsRetriedLater(t *testing.T) {
	cases := map[string]struct {
		failing  string
		attempts int
	}{
		"state unknown":  {showCmd + idleRepo, 1},
		"restart failed": {restart, 2},
	}
	for name, c := range cases {
		failing := c.failing
		t.Run(name, func(t *testing.T) {
			h := newCIHost(t, idleRepo)
			ok := h.script[failing]
			h.script[failing] = procexectest.Reply{Err: errors.New("exit status 1")}
			testutil.True(t, h.sync(t), "the tree installs whatever the restart does")
			testutil.Equal(t, testutil.Read(t, filepath.Join(h.r.Runners.Pending, idleRepo)), "")
			h.script[failing] = ok
			h.sync(t)
			testutil.Missing(t, filepath.Join(h.r.Runners.Pending, idleRepo))
			h.sync(t)
			testutil.Equal(t, h.restarts(idleRepo), c.attempts)
		})
	}
}

func TestFailedSetupLeavesNoRestart(t *testing.T) {
	h := newCIHost(t, idleRepo)
	h.script[setupCmd] = procexectest.Reply{Err: errors.New("exit status 1")}
	_, err := h.r.Sync(context.Background())
	testutil.Is(t, err, ErrRunnerSetup)
	testutil.Equal(t, h.restarts(idleRepo), 0)
	testutil.Missing(t, h.r.Runners.Pending)
}

func TestAnUnrecordableRestartFailsTheSync(t *testing.T) {
	h := newCIHost(t, idleRepo)
	testutil.Write(t, h.r.Runners.Pending, "not a directory")
	_, err := h.r.Sync(context.Background())
	testutil.Fails(t, err)
	testutil.Equal(t, h.exec.Count(setupCmd), 1)
	testutil.Equal(t, h.restarts(idleRepo), 0)
}
