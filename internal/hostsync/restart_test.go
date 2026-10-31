package hostsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	runnerApp  = "tasks"
	runnerName = "runner"
	runnerDst  = policy.RunnerDir + runnerName
	runnerUnit = "runner.service"
	oldTag     = "sha-1111111aaaaaaa"
	newTag     = "sha-2222222bbbbbbb"
	thirdTag   = "sha-3333333ccccccc"
	oldBinary  = "#!/bin/sh\necho old\n"
	newBinary  = "#!/bin/sh\necho new\n"
	gateLines  = "[2026-10-31T00:00:00Z] tasks@1111111 gate ok 60s\n[2026-10-31T00:00:00Z] tasks@2222222 gate ok 60s\n[2026-10-31T00:00:00Z] tasks@3333333 gate ok 60s\n"
)

func runnerSpec() *policy.Host {
	return &policy.Host{
		Files:   []policy.HostFile{{Src: runnerName, Dst: runnerDst, Owner: runnerName, Group: runnerName, Mode: "0755", KeepPrev: true}},
		Restart: &policy.HostRestart{Unit: runnerUnit, Busy: "tasks_runner_active_runs", Version: "tasks_runner_info", Within: 2 * time.Minute},
	}
}

func runnerFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	base := filepath.Dir(f.root)
	testutil.Mkdir(t, f.host(policy.RunnerDir))
	f.files = map[string]string{runnerName: newBinary}
	f.syncer.Policy = policy.Policy{Apps: map[string]policy.App{runnerApp: {Host: runnerSpec()}}}
	f.syncer.Restarts = filepath.Join(base, "restarts")
	appDir := filepath.Join(base, "opt", runnerApp)
	testutil.Write(t, filepath.Join(appDir, "app.conf"), "image="+image+"\n")
	testutil.Write(t, filepath.Join(base, "apps.list"), runnerApp+" "+appDir+"\n")
	testutil.Write(t, filepath.Join(base, "deploys.log"), gateLines)
	return f
}

func (f *fixture) sync(t *testing.T, tag, binary string) error {
	t.Helper()
	f.files[runnerName] = binary
	return f.syncer.Run(context.Background(), runnerApp, tag)
}

func (f *fixture) state(t *testing.T) RestartState {
	t.Helper()
	st, err := LoadState(f.syncer.Restarts, runnerApp)
	testutil.NoErr(t, err)
	return st
}

func TestReplacedBinaryKeepsThePreviousCopyAndWaitsForARestart(t *testing.T) {
	f := runnerFixture(t)
	testutil.Write(t, f.host(runnerDst), oldBinary)
	testutil.Write(t, f.host(runnerDst+".prev-7ecaad5"), "manual copy")
	testutil.NoErr(t, SaveState(f.syncer.Restarts, runnerApp, RestartState{Installed: oldTag}))
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	testutil.Equal(t, testutil.Read(t, f.host(runnerDst)), newBinary)
	testutil.Equal(t, testutil.Read(t, f.host(runnerDst+".prev-1111111")), oldBinary)
	testutil.Perm(t, f.host(runnerDst+".prev-1111111"), 0o600)
	testutil.Missing(t, f.host(runnerDst+".prev-7ecaad5"))
	st := f.state(t)
	testutil.Equal(t, st.Installed, oldTag)
	testutil.True(t, st.Pending != nil && st.Pending.Tag == newTag && st.Pending.PrevTag == oldTag, "pending restart to the new tag")
	testutil.Equal(t, f.exec.Count("systemctl"), 0)
}

func TestFirstReplacementWithoutAKnownVersionKeepsAnUnknownCopy(t *testing.T) {
	f := runnerFixture(t)
	testutil.Write(t, f.host(runnerDst), oldBinary)
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	testutil.Equal(t, testutil.Read(t, f.host(runnerDst+".prev-unknown")), oldBinary)
	testutil.Equal(t, f.state(t).Pending.PrevTag, "")
}

func TestASecondDeployBeforeTheRestartKeepsTheRunningCopy(t *testing.T) {
	f := runnerFixture(t)
	testutil.Write(t, f.host(runnerDst), oldBinary)
	testutil.NoErr(t, SaveState(f.syncer.Restarts, runnerApp, RestartState{Installed: oldTag}))
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	since := f.state(t).Pending.Since
	f.syncer.Now = func() time.Time { return since.Add(time.Hour) }
	testutil.NoErr(t, f.sync(t, thirdTag, "#!/bin/sh\necho third\n"))
	testutil.Equal(t, testutil.Read(t, f.host(runnerDst+".prev-1111111")), oldBinary)
	st := f.state(t)
	testutil.True(t, st.Pending.Tag == thirdTag && st.Pending.PrevTag == oldTag && st.Pending.Since.Equal(since), "the wait keeps its start and its rollback target")
}

func TestUnchangedBinaryAddsNoRestart(t *testing.T) {
	f := runnerFixture(t)
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	testutil.NoErr(t, SaveState(f.syncer.Restarts, runnerApp, RestartState{Installed: newTag}))
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	testutil.True(t, f.state(t).Pending == nil, "nothing to restart")
}

func TestAppsWithoutARestartWriteNoState(t *testing.T) {
	f := newFixture(t)
	f.syncer.Restarts = filepath.Join(t.TempDir(), "restarts")
	f.script[enable] = f.script[reload]
	testutil.NoErr(t, f.run(t))
	testutil.Missing(t, f.syncer.Restarts)
}

func TestBrokenStateStopsTheSyncBeforeAnyChange(t *testing.T) {
	f := runnerFixture(t)
	testutil.Write(t, f.host(runnerDst), oldBinary)
	testutil.Write(t, statePath(f.syncer.Restarts, runnerApp), "{")
	testutil.Fails(t, f.sync(t, newTag, newBinary))
	testutil.Equal(t, testutil.Read(t, f.host(runnerDst)), oldBinary)
}

func TestUnwritableStateDirectoryStopsTheSync(t *testing.T) {
	f := runnerFixture(t)
	testutil.Write(t, f.syncer.Restarts, "a file, not a directory")
	testutil.Fails(t, f.sync(t, newTag, newBinary))
	testutil.Missing(t, f.host(runnerDst))
}

func TestStateLockMakesTheSyncWait(t *testing.T) {
	dir := t.TempDir()
	release, err := LockState(context.Background(), dir, runnerApp, 0)
	testutil.NoErr(t, err)
	_, err = LockState(context.Background(), dir, runnerApp, 0)
	testutil.Fails(t, err)
	release()
	again, err := LockState(context.Background(), dir, runnerApp, 0)
	testutil.NoErr(t, err)
	again()
}

func TestSymlinkedRunnerDirectoryIsRefused(t *testing.T) {
	f := runnerFixture(t)
	elsewhere := t.TempDir()
	testutil.NoErr(t, os.RemoveAll(f.host(policy.RunnerDir)))
	testutil.NoErr(t, os.Symlink(elsewhere, strings.TrimSuffix(f.host(policy.RunnerDir), "/")))
	testutil.Is(t, f.sync(t, newTag, newBinary), ErrParent)
	entries, _ := os.ReadDir(elsewhere)
	testutil.Equal(t, len(entries), 0)
}

func TestWritesThroughASwappedDirectoryAreRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	testutil.Mkdir(t, dir)
	elsewhere := t.TempDir()
	testutil.NoErr(t, os.RemoveAll(dir))
	testutil.NoErr(t, os.Symlink(elsewhere, dir))
	s := Syncer{Chown: (*os.File).Chown}
	testutil.Is(t, s.writeFile(filepath.Join(dir, runnerName), snapshot{body: bytesBody([]byte("x")), mode: 0o755, uid: os.Getuid(), gid: os.Getgid()}), ErrParent)
	testutil.Is(t, removeFile(filepath.Join(dir, runnerName)), ErrParent)
	_, err := (body{dir: dir, path: runnerName}).open()
	testutil.Is(t, err, ErrParent)
}

func TestPrevNameUsesTheShortSha(t *testing.T) {
	testutil.Equal(t, PrevName("/x/runner", oldTag), "/x/runner.prev-1111111")
	testutil.Equal(t, PrevName("/x/runner", ""), "/x/runner.prev-unknown")
}

func TestHostSyncJournalStaysTheSameForRestartApps(t *testing.T) {
	f := runnerFixture(t)
	testutil.NoErr(t, f.sync(t, newTag, newBinary))
	testutil.Contains(t, f.journal(t), "tasks@2222222 "+journal.ActionHostSync+" ok", "changed=1")
}
