package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	goBin     = "go"
	webModule = "example.com/web\n"
)

type chunkDirs struct {
	spool, stash, work, yield string
}

func executorHarness(t *testing.T) (*harness, chunkDirs) {
	t.Helper()
	h := newHarness(t)
	root := t.TempDir()

	h.env[envSpoolRoot] = filepath.Join(root, "spool")
	h.write(filepath.Join(h.hubDir, "apps.list"), appWeb+"\n")
	h.write(filepath.Join(filepath.Dir(h.hubDir), appWeb, "app.conf"), "port=9001\nimage=ghcr.io/o/web\nrepo=DreamsAreReal/web\n")
	home := filepath.Join(h.hubDir, "ci", appWeb, "mutation")
	d := chunkDirs{spool: filepath.Join(h.env[envSpoolRoot], appWeb), stash: filepath.Join(home, "stash"), work: filepath.Join(home, "work"), yield: filepath.Join(root, "job-running")}
	h.write(filepath.Join(d.stash, commitFile), commitSha+"\n")
	h.write(filepath.Join(d.stash, policyFile), testutil.Read(t, gatePolicy(t)))
	return h, d
}

func (d chunkDirs) args() []string {
	return []string{CmdChunk, "--spool", d.spool, "--stash", d.stash, "--work", d.work, "--yield", d.yield, "--budget", "5m"}
}

func chunkCheck(t *testing.T, seen *gate.MutationPlan, run mutation.RunSummary) []gate.Check {
	t.Helper()
	return []gate.Check{{Name: pack.CheckMutation, Run: func(_ context.Context, ws gate.Workspace) gate.Outcome {
		*seen = ws.Mutation
		v := mutation.Verdicts{Version: 1, Bases: []string{verdictKey}, Entries: map[string]mutation.Status{verdictKey: mutation.StatusKilled}}
		testutil.Mkdir(t, filepath.Join(ws.Out, "go-app"))
		_, err := v.Save(filepath.Join(ws.Out, "go-app", mutation.VerdictsFile))
		testutil.NoErr(t, err)
		data, err := json.Marshal(mutation.ProjectRun{Base: verdictKey, Complete: run.Complete, Passed: run.Passed, Fresh: true})
		testutil.NoErr(t, err)
		testutil.Write(t, filepath.Join(ws.Out, "go-app", mutation.RunFile), string(data))
		return gate.Pass(nil)
	}}}
}

func readJob(t *testing.T, d chunkDirs) runner.FullJob {
	t.Helper()
	var job runner.FullJob
	testutil.NoErr(t, json.Unmarshal([]byte(testutil.Read(t, filepath.Join(d.spool, jobFile))), &job))
	return job
}

func TestServerFullRunChunkEndToEnd(t *testing.T) {
	h, d := executorHarness(t)
	var calls []string
	h.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		calls = append(calls, c.Name+" "+strings.Join(c.Args, " "))
		if c.Name == goBin {
			return procexec.Result{Stdout: webModule}, nil
		}
		return procexec.Result{}, nil
	}
	h.want(ExitOK, CmdDispatch, appWeb)
	testutil.Contains(t, h.out.String(), "mutation-dispatch: web plan=new")
	testutil.Equal(t, readJob(t, d), runner.FullJob{Plan: "new", App: appWeb, Repo: "DreamsAreReal/web"})
	testutil.Contains(t, strings.Join(h.grouped, " "), "ci-web:"+d.spool, "ci-web:"+filepath.Join(d.spool, jobFile)+".tmp")
	var seen gate.MutationPlan
	h.gateChecks = chunkCheck(t, &seen, mutation.RunSummary{})
	testutil.Equal(t, h.run(d.args()...), ExitOK)
	testutil.True(t, seen.Full && seen.Yield == d.yield && seen.Cache == "", "a first chunk mutates without a cache: "+h.out.String()+h.err.String())
	testutil.Contains(t, strings.Join(calls, "\n"), "tar -xf "+filepath.Join(d.stash, srcArchive)+" -C "+filepath.Join(d.work, chunkSrc), "git init -q", "git add -A")
	testutil.Equal(t, testutil.Read(t, filepath.Join(d.work, chunkOut, commitFile)), commitSha+"\n")
	h.want(ExitOK, CmdImport, appWeb)
	testutil.Contains(t, h.out.String(), "mutation-import: web chunk of "+commitSha+" taken")
	testutil.Missing(t, filepath.Join(d.work, chunkOut, commitFile))
	h.want(ExitOK, CmdDispatch, appWeb)
	job := readJob(t, d)
	testutil.True(t, job.Plan == "continue" && job.SessionSHA256 == mutation.Digest([]byte(testutil.Read(t, filepath.Join(d.spool, sessionFile)))), "the next chunk continues with the session")
	h.gateChecks = chunkCheck(t, &seen, mutation.RunSummary{Complete: true, Passed: true})
	testutil.Equal(t, h.run(d.args()...), ExitOK)
	testutil.Equal(t, seen.CacheSHA256, job.SessionSHA256)
	h.want(ExitOK, CmdImport, appWeb)
	h.want(ExitOK, CmdDispatch, appWeb)
	testutil.Equal(t, readJob(t, d).Plan, "skip")
	testutil.Missing(t, filepath.Join(d.spool, sessionFile))
	testutil.Equal(t, h.run(d.args()...), ExitOK)
	testutil.Contains(t, h.out.String(), "mutation-run: nothing to do (plan skip)")
	h.want(ExitOK, CmdImport, appWeb)
	testutil.Contains(t, h.out.String(), "mutation-import: web has no chunk to take")
	h.want(ExitOK, CmdDispatch, appWeb)
	testutil.Equal(t, readJob(t, d).Plan, "skip")
}

func TestServerChunkSkipsAndRefuses(t *testing.T) {
	h, d := executorHarness(t)
	h.want(ExitOK, d.args()...)
	testutil.Contains(t, h.out.String(), "nothing to do (no job dispatched)")
	h.want(ExitOK, CmdDispatch, appWeb)
	h.write(d.yield, "")
	h.want(ExitOK, d.args()...)
	testutil.Contains(t, h.out.String(), "nothing to do (the CI runner has a job)")
	testutil.NoErr(t, os.Remove(d.yield))
	h.write(filepath.Join(d.stash, commitFile), "not-a-commit\n")
	h.want(ExitFail, d.args()...)
	h.write(filepath.Join(d.stash, commitFile), commitSha+"\n")
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	h.want(ExitFail, d.args()...)
	testutil.Contains(t, h.err.String(), "mutation-run: tar -xf")
	h.want(ExitUsage, CmdChunk, "--spool", "relative")
	h.want(ExitUsage, CmdChunk, "extra")
	h.write(filepath.Join(d.spool, jobFile), "{")
	h.want(ExitFail, d.args()...)
}

func TestServerImportRefusesForgedOrMissingChunks(t *testing.T) {
	h, d := executorHarness(t)
	h.want(ExitOK, CmdImport, appWeb)
	testutil.Contains(t, h.out.String(), "mutation-import: web has no chunk to take")
	out := filepath.Join(d.work, chunkOut)
	h.write(filepath.Join(out, commitFile), commitSha+"\n")
	h.want(ExitOK, CmdImport, appWeb)
	h.write(filepath.Join(out, mutation.VerdictsFile), `{"version":1,"entries":{"k":"killed"}}`)
	h.write(filepath.Join(out, mutation.RunFile), `{}`)
	h.want(ExitFail, CmdImport, appWeb)
	testutil.Contains(t, h.err.String(), "full mutation chunk rejected")
	testutil.NoErr(t, os.Remove(filepath.Join(out, mutation.VerdictsFile)))
	testutil.NoErr(t, os.Symlink("/etc/passwd", filepath.Join(out, mutation.VerdictsFile)))
	h.want(ExitFail, CmdImport, appWeb)
	testutil.Contains(t, h.err.String(), "must be a regular file")
	h.want(ExitUsage, CmdImport)
	h.want(ExitUsage, CmdDispatch, "Bad!")
	h.want(ExitFail, CmdDispatch, "ghost")
}

func TestServerDispatchSpoolFailures(t *testing.T) {
	h, _ := executorHarness(t)
	h.groupErr = errSim
	h.want(ExitFail, CmdDispatch, appWeb)
	testutil.Contains(t, h.err.String(), "mutation-dispatch: ")
}

func TestDispatchFailsWhenTheSpoolCannotBeWritten(t *testing.T) {
	h, d := executorHarness(t)
	h.write(filepath.Dir(d.spool), "not a directory")
	h.want(ExitFail, CmdDispatch, appWeb)
	testutil.Contains(t, h.err.String(), dispatchPrefix)

	h, d = executorHarness(t)
	testutil.Mkdir(t, filepath.Join(d.spool, jobFile+".tmp", "busy"))
	h.want(ExitFail, CmdDispatch, appWeb)
	testutil.Contains(t, h.err.String(), dispatchPrefix)
	_, err := os.Stat(filepath.Join(d.spool, jobFile))
	testutil.True(t, errors.Is(err, fs.ErrNotExist), "a failed write leaves no job behind")
}

func TestSpoolRootStaysTraversable(t *testing.T) {
	h, d := executorHarness(t)
	h.write(filepath.Join(d.spool, jobFile+".tmp"), "stale")
	testutil.NoErr(t, os.Chmod(filepath.Join(d.spool, jobFile+".tmp"), 0o600))
	testutil.NoErr(t, os.Chmod(d.spool, 0o600))
	testutil.NoErr(t, os.Chmod(filepath.Dir(d.spool), 0o600))
	h.want(ExitOK, CmdDispatch, appWeb)
	root, err := os.Stat(filepath.Dir(d.spool))
	testutil.NoErr(t, err)
	testutil.Equal(t, root.Mode().Perm(), os.FileMode(spoolRootPerm))
	app, err := os.Stat(d.spool)
	testutil.NoErr(t, err)
	testutil.Equal(t, app.Mode().Perm(), os.FileMode(spoolDirPerm))
	job, err := os.Stat(filepath.Join(d.spool, jobFile))
	testutil.NoErr(t, err)
	testutil.Equal(t, job.Mode().Perm(), os.FileMode(spoolFilePerm))
}
