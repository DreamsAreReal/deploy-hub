package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	hookName  = "ci-job-started"
	unitName  = "ci-runner@.service"
	setupName = "ci-runner-setup"
	newHook   = "#!/bin/sh\necho allow pull_request\n"
	oldHook   = "#!/bin/sh\necho push only\n"
	unitBody  = "[Service]\nExecStart=/bin/true\n"
	setupBody = "#!/bin/sh\nexit 0\n"
)

func runnerTree() fstest.MapFS {
	return fstest.MapFS{
		hookName:  {Data: []byte(newHook)},
		unitName:  {Data: []byte(unitBody)},
		setupName: {Data: []byte(setupBody)},
	}
}

type runnerFixture struct {
	r      RunnerFiles
	exec   *procexectest.Fake
	script procexectest.Script
	seen   []string
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	f := &runnerFixture{script: procexectest.Script{setupCmd: {}}}
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		f.seen = append(f.seen, testutil.Read(t, filepath.Join(f.r.Dir, hookName)))
		return f.script.Handle(c)
	}}
	f.r = RunnerFiles{Files: runnerTree(), Dir: filepath.Join(t.TempDir(), "ci-runner"), Exec: f.exec, Runners: idleHost(t, f.exec)}
	return f
}

func (f *runnerFixture) installOld(t *testing.T) {
	t.Helper()
	testutil.Mkdir(t, f.r.Dir)
	testutil.Write(t, filepath.Join(f.r.Dir, hookName), oldHook)
	testutil.Write(t, filepath.Join(f.r.Dir, unitName), unitBody)
	testutil.Write(t, filepath.Join(f.r.Dir, setupName), setupBody)
	testutil.NoErr(t, os.Chmod(filepath.Join(f.r.Dir, hookName), scriptPerm))
	testutil.NoErr(t, os.Chmod(filepath.Join(f.r.Dir, setupName), scriptPerm))
}

func TestRunnerFilesInstallTheEmbeddedTreeAndRunSetupOnce(t *testing.T) {
	f := newRunnerFixture(t)
	changed, err := f.r.Sync(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, changed, "first sync installs")
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.r.Dir, hookName)), newHook)
	testutil.Perm(t, filepath.Join(f.r.Dir, hookName), scriptPerm)
	testutil.Perm(t, filepath.Join(f.r.Dir, setupName), scriptPerm)
	testutil.Perm(t, filepath.Join(f.r.Dir, unitName), dataPerm)
	testutil.Equal(t, f.exec.Lines(), []string{setupCmd})
	testutil.Missing(t, f.r.Dir+candidateSuffix)
	changed, err = f.r.Sync(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, !changed, "same tree is not installed again")
	testutil.Equal(t, f.exec.Count(setupCmd), 1)
}

func TestRunnerFilesReplaceAnyDrift(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"changed hook": func(t *testing.T, dir string) {},
		"extra file":   func(t *testing.T, dir string) { testutil.Write(t, filepath.Join(dir, "planted"), "x") },
		"lost exec bit": func(t *testing.T, dir string) {
			testutil.Write(t, filepath.Join(dir, hookName), newHook)
			testutil.NoErr(t, os.Chmod(filepath.Join(dir, hookName), dataPerm))
		},
		"symlinked unit": func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "unit")
			testutil.Write(t, target, unitBody)
			testutil.NoErr(t, os.Remove(filepath.Join(dir, unitName)))
			testutil.NoErr(t, os.Symlink(target, filepath.Join(dir, unitName)))
		},
		"missing file": func(t *testing.T, dir string) {
			testutil.NoErr(t, os.Remove(filepath.Join(dir, unitName)))
			testutil.Write(t, filepath.Join(dir, "other"), unitBody)
		},
	}
	for name, drift := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRunnerFixture(t)
			f.installOld(t)
			drift(t, f.r.Dir)
			changed, err := f.r.Sync(context.Background())
			testutil.NoErr(t, err)
			testutil.True(t, changed, "drift is replaced")
			testutil.Equal(t, testutil.Read(t, filepath.Join(f.r.Dir, hookName)), newHook)
			testutil.Perm(t, filepath.Join(f.r.Dir, hookName), scriptPerm)
			testutil.Missing(t, filepath.Join(f.r.Dir, "planted"))
			testutil.Equal(t, f.seen, []string{newHook})
			changed, err = f.r.Sync(context.Background())
			testutil.NoErr(t, err)
			testutil.True(t, !changed, "installed tree is stable")
		})
	}
}

func TestFailedSetupPutsThePreviousTreeBackAndAppliesIt(t *testing.T) {
	f := newRunnerFixture(t)
	f.installOld(t)
	f.exec.Handler = func(procexec.Command) (procexec.Result, error) {
		hook := testutil.Read(t, filepath.Join(f.r.Dir, hookName))
		f.seen = append(f.seen, hook)
		if hook == newHook {
			return procexec.Result{}, errors.New("exit status 1")
		}
		return procexec.Result{}, nil
	}
	changed, err := f.r.Sync(context.Background())
	testutil.Is(t, err, ErrRunnerSetup)
	testutil.True(t, !changed, "failed sync reports no change")
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.r.Dir, hookName)), oldHook)
	testutil.Equal(t, f.seen, []string{newHook, oldHook})
	testutil.Missing(t, f.r.Dir+previousSuffix)
}

func TestFailedRestoreIsReported(t *testing.T) {
	f := newRunnerFixture(t)
	f.installOld(t)
	f.script[setupCmd] = procexectest.Reply{Err: errors.New("exit status 1")}
	_, err := f.r.Sync(context.Background())
	testutil.Is(t, err, ErrRunnerSetup)
	testutil.Equal(t, f.seen, []string{newHook, oldHook})
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.r.Dir, hookName)), oldHook)
}

func TestFailedFirstSetupLeavesNoTree(t *testing.T) {
	f := newRunnerFixture(t)
	f.script[setupCmd] = procexectest.Reply{Err: errors.New("exit status 1")}
	_, err := f.r.Sync(context.Background())
	testutil.Is(t, err, ErrRunnerSetup)
	testutil.Missing(t, f.r.Dir)
	testutil.Equal(t, f.exec.Count(setupCmd), 1)
}

func TestUnwritableRunnerDirectoryFailsBeforeSetup(t *testing.T) {
	f := newRunnerFixture(t)
	f.r.Dir = filepath.Join(t.TempDir(), "missing", "ci-runner")
	_, err := f.r.Sync(context.Background())
	testutil.Fails(t, err)
	testutil.Equal(t, f.exec.Count(setupCmd), 0)
	g := newRunnerFixture(t)
	g.r.Files = fstest.MapFS{"dir/file": {Data: []byte("x")}}
	_, err = g.r.Sync(context.Background())
	testutil.Fails(t, err)
	testutil.Missing(t, g.r.Dir+candidateSuffix)
	testutil.Equal(t, g.exec.Count(setupCmd), 0)
}

func TestRunnerSyncFailureOfTheNewBinaryRestoresTheOldOne(t *testing.T) {
	f := newFixture(t)
	f.script[runnerCmd] = procexectest.Reply{Err: errors.New("ci-runner-setup host failed")}
	testutil.Is(t, f.u.Run(context.Background(), ""), ErrUpdate)
	testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
	testutil.Equal(t, f.exec.Count(runnerCmd), 2)
	testutil.Contains(t, f.journal(t), "deploy-hub@2222222 self-update fail", "restored=fail")
}
