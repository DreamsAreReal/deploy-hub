package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	"github.com/DreamsAreReal/deploy-hub/internal/tools"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	envSpoolRoot   = "DEPLOY_HUB_MUTATION_SPOOL"
	defaultSpool   = "/var/lib/ci-mutation"
	ciUserPrefix   = "ci-"
	jobFile        = "job.json"
	sessionFile    = "session.json"
	commitFile     = "commit"
	srcArchive     = "src.tar"
	policyFile     = "policy/gate.yaml"
	chunkWork      = "mutation/work"
	chunkOut       = "out"
	chunkSrc       = "src"
	spoolDirPerm   = 0o750
	spoolRootPerm  = 0o755
	spoolFilePerm  = 0o640
	usageDispatch  = "usage: deploy-hub mutation-dispatch <app>"
	usageImport    = "usage: deploy-hub mutation-import <app>"
	usageChunk     = "usage: deploy-hub mutation-run --spool DIR --stash DIR --work DIR --yield FILE [--budget DURATION]"
	dispatchPrefix = "mutation-dispatch: "
	importPrefix   = "mutation-import: "
	chunkPrefix    = "mutation-run: "
	msgPlanned     = "mutation-dispatch: %s plan=%s\n"
	msgImported    = "mutation-import: %s chunk of %s taken\n"
	msgNoChunk     = "mutation-import: %s has no chunk to take\n"
	msgChunkSkip   = "mutation-run: nothing to do (%s)\n"
	defaultBudget  = 45 * time.Minute
)

var chunkBinaries = append([]string{tools.TarBinary}, gateBinaries...)

func appArg(args []string, usageText string) (string, error) {
	if len(args) != 1 || validate.AppName(args[0]) != nil {
		return "", errors.New(usageText)
	}
	return args[0], nil
}

func runMutationDispatch(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	app, err := appArg(args, usageDispatch)
	if err != nil {
		a.Err.Println(err.Error())
		return ExitUsage
	}
	job, session, err := a.runnerService(ex).DispatchFull(ctx, app)
	if err == nil {
		err = a.writeSpool(filepath.Join(a.Config.SpoolRoot, app), ciUserPrefix+app, job, session)
	}
	if err != nil {
		return a.failWith(dispatchPrefix, err)
	}
	a.Out.Printf(msgPlanned, app, job.Plan)
	return ExitOK
}

func (a *App) writeSpool(dir, group string, job runner.FullJob, session []byte) error {
	data, err := json.Marshal(job)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(dir), spoolRootPerm)
	}
	if err == nil {
		err = os.Mkdir(dir, spoolDirPerm)
	}
	if errors.Is(err, fs.ErrExist) {
		err = nil
	}
	if err == nil {
		err = a.Group(group)(dir)
	}
	if err == nil {
		err = a.spoolFile(filepath.Join(dir, jobFile), group, data)
	}
	if err == nil && session != nil {
		err = a.spoolFile(filepath.Join(dir, sessionFile), group, session)
	}
	if err == nil && session == nil {
		err = removeIfPresent(filepath.Join(dir, sessionFile))
	}
	return err
}

func (a *App) spoolFile(path, group string, data []byte) error {
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, data, spoolFilePerm)
	if err == nil {
		err = a.Group(group)(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	return err
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func runMutationImport(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	app, err := appArg(args, usageImport)
	if err != nil {
		a.Err.Println(err.Error())
		return ExitUsage
	}
	files, err := readChunk(filepath.Join(a.CIRoot, app, chunkWork, chunkOut))
	if errors.Is(err, fs.ErrNotExist) {
		a.Out.Printf(msgNoChunk, app)
		return ExitOK
	}
	if err == nil {
		err = a.runnerService(ex).ImportFull(ctx, app, strings.TrimSpace(string(files[0])), files[1], files[2])
	}
	if err != nil {
		return a.failWith(importPrefix, err)
	}
	a.Out.Printf(msgImported, app, strings.TrimSpace(string(files[0])))
	return ExitOK
}

func readChunk(dir string) ([][]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := make([][]byte, 0, len(chunkFiles))
	for _, name := range chunkFiles {
		data, rerr := readLimited(root, name)
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, data)
	}
	return out, nil
}

var chunkFiles = []string{commitFile, mutation.VerdictsFile, mutation.RunFile}

func readLimited(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > runner.MaxCacheBytes {
		return nil, fmt.Errorf("%w: %s must be a regular file of at most %d bytes", runner.ErrImport, name, runner.MaxCacheBytes)
	}
	return root.ReadFile(name)
}

type chunkArgs struct {
	spool, stash, work, yield string
	budget                    time.Duration
}

func parseChunkArgs(args []string) (chunkArgs, error) {
	var c chunkArgs
	fs := flag.NewFlagSet(CmdChunk, flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	fs.StringVar(&c.spool, "spool", "", "the job the hub dispatched")
	fs.StringVar(&c.stash, "stash", "", "the source and toolchain the last deploy kept")
	fs.StringVar(&c.work, "work", "", "work directory, emptied first")
	fs.StringVar(&c.yield, "yield", "", "the file the CI runner creates while it runs a job")
	fs.DurationVar(&c.budget, "budget", defaultBudget, "how long this chunk may mutate")
	err := fs.Parse(args)
	for _, p := range []string{c.spool, c.stash, c.work, c.yield} {
		if err == nil && !filepath.IsAbs(p) {
			err = errors.New(usageChunk)
		}
	}
	if err != nil || fs.NArg() != 0 {
		return c, errors.New(usageChunk)
	}
	return c, nil
}

func runMutationRun(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	c, err := parseChunkArgs(args)
	if err != nil {
		a.Err.Println(err.Error())
		return ExitUsage
	}
	job, commit, reason, err := c.ready()
	if err != nil {
		return a.failWith(chunkPrefix, err)
	}
	if reason != "" {
		a.Out.Printf(msgChunkSkip, reason)
		return ExitOK
	}
	if err := c.stage(ctx, ex); err != nil {
		return a.failWith(chunkPrefix, err)
	}
	code := runGate(ctx, a, ex, c.gateArgs(job, commit))
	out := filepath.Join(c.work, chunkOut)
	if !exists(filepath.Join(out, mutation.VerdictsFile)) {
		return code
	}
	if err := os.WriteFile(filepath.Join(out, commitFile), []byte(commit+"\n"), spoolFilePerm); err != nil {
		return a.failWith(chunkPrefix, err)
	}
	return ExitOK
}

func (c chunkArgs) ready() (runner.FullJob, string, string, error) {
	var job runner.FullJob
	data, err := os.ReadFile(filepath.Join(c.spool, jobFile))
	if errors.Is(err, fs.ErrNotExist) {
		return job, "", "no job dispatched", nil
	}
	if err == nil {
		err = json.Unmarshal(data, &job)
	}
	var commit []byte
	if err == nil {
		commit, err = os.ReadFile(filepath.Join(c.stash, commitFile))
	}
	if err != nil {
		return job, "", "", err
	}
	return job, strings.TrimSpace(string(commit)), c.skipReason(job), validate.Commit(strings.TrimSpace(string(commit)))
}

func (c chunkArgs) skipReason(job runner.FullJob) string {
	switch {
	case !job.Runs():
		return "plan " + job.Plan
	case exists(c.yield):
		return "the CI runner has a job"
	}
	return ""
}

func (c chunkArgs) stage(ctx context.Context, ex procexec.Runner) error {
	src := filepath.Join(c.work, chunkSrc)
	if err := os.RemoveAll(c.work); err != nil {
		return err
	}
	if err := os.MkdirAll(src, outDirPerm); err != nil {
		return err
	}
	steps := []procexec.Command{
		{Name: tools.TarBinary, Args: []string{"-xf", filepath.Join(c.stash, srcArchive), "-C", src}},
		{Name: source.GitBinary, Args: []string{"init", "-q"}, Dir: src},
		{Name: source.GitBinary, Args: []string{"add", "-A"}, Dir: src},
	}
	for _, s := range steps {
		if _, err := ex.Run(ctx, s); err != nil {
			return fmt.Errorf("%s %s: %w: %s", s.Name, strings.Join(s.Args, " "), err, procexec.Output(err))
		}
	}
	return nil
}

func (c chunkArgs) gateArgs(job runner.FullJob, commit string) []string {
	args := []string{"--app", job.App, "--repo", job.Repo, "--commit", commit, "--policy", filepath.Join(c.stash, policyFile),
		"--src", filepath.Join(c.work, chunkSrc), "--out", filepath.Join(c.work, chunkOut),
		"--mutation-full", "--mutation-budget", c.budget.String(), "--mutation-yield", c.yield}
	if job.SessionSHA256 != "" {
		args = append(args, "--mutation-cache", filepath.Join(c.spool, sessionFile), "--mutation-cache-sha256", digestPrefix+job.SessionSHA256)
	}
	return args
}
