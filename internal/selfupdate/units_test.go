package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	serviceName = "deploy-hub-history-guard.service"
	timerName   = "deploy-hub-history-guard.timer"
	serviceBody = "[Service]\nExecStart=/opt/deploy-hub/bin/deploy-hub history-sweep\n"
	oldService  = "[Service]\nExecStart=/usr/bin/curl dispatch\n"
	timerBody   = "[Timer]\nOnUnitActiveSec=5min\n"
	foreignName = "caddy.service"
	analyze     = AnalyzeCommand + " verify"
	reload      = SystemctlCommand + " daemon-reload"
	enable      = SystemctlCommand + " enable --now"
)

func unitTree() fstest.MapFS {
	return fstest.MapFS{serviceName: {Data: []byte(serviceBody)}, timerName: {Data: []byte(timerBody)}}
}

type unitFixture struct {
	u      UnitFiles
	exec   *procexectest.Fake
	script procexectest.Script
}

func newUnitFixture(t *testing.T) *unitFixture {
	t.Helper()
	f := &unitFixture{script: procexectest.Script{analyze: {}, reload: {}, enable: {}}}
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) { return f.script.Handle(c) }}
	f.u = UnitFiles{Files: unitTree(), Dir: t.TempDir(), Exec: f.exec}
	testutil.Write(t, f.path(foreignName), "[Service]\n")
	return f
}

func (f *unitFixture) path(name string) string { return filepath.Join(f.u.Dir, name) }

func (f *unitFixture) installOld(t *testing.T) {
	t.Helper()
	testutil.Write(t, f.path(serviceName), oldService)
	testutil.Write(t, f.path(timerName), timerBody)
	testutil.NoErr(t, os.Chmod(f.path(serviceName), unitPerm))
	testutil.NoErr(t, os.Chmod(f.path(timerName), unitPerm))
}

func TestUnitFilesInstallVerifyReloadAndEnableTimers(t *testing.T) {
	f := newUnitFixture(t)
	changed, err := f.u.Sync(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, changed, "first sync installs")
	testutil.Equal(t, testutil.Read(t, f.path(serviceName)), serviceBody)
	testutil.Perm(t, f.path(serviceName), unitPerm)
	testutil.Perm(t, f.path(timerName), unitPerm)
	testutil.Equal(t, f.exec.Lines(), []string{
		analyze + " " + f.path(serviceName) + " " + f.path(timerName), reload, enable + " " + timerName,
	})
	testutil.Equal(t, testutil.Read(t, f.path(foreignName)), "[Service]\n")
	testutil.Missing(t, f.path("."+serviceName+unitTempSuffix))
	changed, err = f.u.Sync(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, !changed, "installed units are stable")
	testutil.Equal(t, len(f.exec.Lines()), 3)
}

func TestUnitFilesReplaceOnlyTheDriftedUnit(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"changed content": func(*testing.T, string) {},
		"wrong mode": func(t *testing.T, path string) {
			testutil.Write(t, path, serviceBody)
			testutil.NoErr(t, os.Chmod(path, 0o600))
		},
		"symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "unit")
			testutil.Write(t, target, serviceBody)
			testutil.NoErr(t, os.Remove(path))
			testutil.NoErr(t, os.Symlink(target, path))
		},
	}
	for name, drift := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUnitFixture(t)
			f.installOld(t)
			drift(t, f.path(serviceName))
			changed, err := f.u.Sync(context.Background())
			testutil.NoErr(t, err)
			testutil.True(t, changed, "drift is replaced")
			testutil.Equal(t, testutil.Read(t, f.path(serviceName)), serviceBody)
			testutil.Perm(t, f.path(serviceName), unitPerm)
			testutil.Equal(t, f.exec.Lines(), []string{analyze + " " + f.path(serviceName), reload})
		})
	}
}

func TestFailedActivationRestoresThePreviousUnits(t *testing.T) {
	for _, step := range []string{analyze, reload, enable} {
		t.Run(step, func(t *testing.T) {
			f := newUnitFixture(t)
			testutil.Write(t, f.path(serviceName), oldService)
			failed := false
			f.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
				if !failed && strings.HasPrefix(procexectest.Line(c), step) {
					failed = true
					return procexec.Result{}, errors.New("bad unit")
				}
				return f.script.Handle(c)
			}
			changed, err := f.u.Sync(context.Background())
			testutil.Is(t, err, ErrUnitActivate)
			testutil.Contains(t, err.Error(), step, "bad unit")
			testutil.True(t, !changed, "failed sync reports no change")
			testutil.Equal(t, testutil.Read(t, f.path(serviceName)), oldService)
			testutil.Missing(t, f.path(timerName))
			lines := f.exec.Lines()
			testutil.Equal(t, lines[len(lines)-1], reload)
		})
	}
}

func TestRestoreReportsAFailedReload(t *testing.T) {
	f := newUnitFixture(t)
	f.script[analyze] = procexectest.Reply{Err: errors.New("bad unit")}
	f.script[reload] = procexectest.Reply{Err: errors.New("dbus gone")}
	_, err := f.u.Sync(context.Background())
	testutil.Contains(t, err.Error(), "bad unit", "daemon-reload: dbus gone")
}

func TestUnwritableUnitDirFailsAndKeepsTheUnits(t *testing.T) {
	f := newUnitFixture(t)
	f.u.Dir = filepath.Join(f.u.Dir, "absent")
	_, err := f.u.Sync(context.Background())
	testutil.Is(t, err, ErrUnitActivate)
	testutil.Equal(t, len(f.exec.Lines()), 1)
}

type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestUnreadableUnitsFailThePlanBeforeAnyStep(t *testing.T) {
	cases := map[string]func(f *unitFixture){
		"installed unit is a directory": func(f *unitFixture) { testutil.NoErr(t, os.Mkdir(f.path(serviceName), 0o700)) },
		"unit dir is a file": func(f *unitFixture) {
			f.u.Dir = filepath.Join(f.u.Dir, foreignName)
		},
		"embedded tree unreadable": func(f *unitFixture) { f.u.Files = brokenFS{} },
		"embedded entry is a directory": func(f *unitFixture) {
			f.u.Files = fstest.MapFS{"sub/" + serviceName: {Data: []byte(serviceBody)}}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUnitFixture(t)
			dir := f.u.Dir
			breakIt(f)
			changed, err := f.u.Sync(context.Background())
			testutil.Fails(t, err)
			testutil.True(t, !changed, "a failed plan changes nothing")
			testutil.Equal(t, len(f.exec.Lines()), 0)
			testutil.Equal(t, testutil.Read(t, filepath.Join(dir, foreignName)), "[Service]\n")
		})
	}
}

func TestWriteUnitLeavesNoTempFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, serviceName)
	testutil.Mkdir(t, filepath.Join(target, "keep"))
	testutil.Fails(t, writeUnit(target, []byte(serviceBody)))
	testutil.Missing(t, filepath.Join(dir, unitTempPrefix+serviceName+unitTempSuffix))
}

func TestRestoreRemovesNewUnitsAndReportsWhatItCannotRemove(t *testing.T) {
	f := newUnitFixture(t)
	gone := unitChange{path: f.path(timerName)}
	testutil.NoErr(t, f.u.restore(context.Background(), []unitChange{gone}))
	testutil.Write(t, f.path(timerName), timerBody)
	testutil.NoErr(t, f.u.restore(context.Background(), []unitChange{gone}))
	testutil.Missing(t, f.path(timerName))
	testutil.Mkdir(t, filepath.Join(f.path(timerName), "keep"))
	testutil.Fails(t, f.u.restore(context.Background(), []unitChange{gone}))
	testutil.Equal(t, f.exec.Count(reload), 3)
}

func TestUpToDateRunInstallsChangedUnitsAlone(t *testing.T) {
	f := newFixture(t)
	f.u.Current = newCommit
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Write(t, filepath.Join(f.u.Units.Dir, serviceName), oldService)
	f.metrics.samples = nil
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 1.0)
	testutil.Equal(t, f.exec.Count(setupCmd), 1)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.u.Units.Dir, serviceName)), serviceBody)
}

func TestUpToDateRunFailsWhenTheHostFilesCannotBeInstalled(t *testing.T) {
	for name, breakIt := range map[string]func(u *Updater){
		"units":  func(u *Updater) { u.Units.Dir = filepath.Join(u.Units.Dir, "absent") },
		"runner": func(u *Updater) { u.Runner.Dir = filepath.Join(u.Runner.Dir, "absent", "ci-runner") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.u.Current = newCommit
			breakIt(&f.u)
			testutil.Fails(t, f.u.Run(context.Background(), ""))
			testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 0.0)
			testutil.Contains(t, f.journal(t), "deploy-hub@2222222 self-update fail")
		})
	}
}
