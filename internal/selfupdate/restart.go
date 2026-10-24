package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DefaultRestartDir = "/opt/deploy-hub/ci-runner-restart"
	DefaultWantsDir   = "/etc/systemd/system/multi-user.target.wants"
	DefaultCgroupRoot = "/sys/fs/cgroup"
	DefaultProcRoot   = "/proc"
	runnerUnitPrefix  = "ci-runner@"
	dockerUnitPrefix  = "ci-docker@"
	unitSuffix        = ".service"
	jobProcess        = "Runner.Worker"
	cgroupProcs       = "cgroup.procs"
	markerPerm        = 0o600
)

type Runners struct {
	Pending string
	Wants   string
	Cgroups string
	Proc    string
	Exec    procexec.Runner
	Log     *slog.Logger
}

func (r Runners) MarkAll() error {
	links, err := filepath.Glob(filepath.Join(r.Wants, runnerUnitPrefix+"*"+unitSuffix))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.Pending, dirPerm); err != nil {
		return err
	}
	for _, link := range links {
		repo := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(link), runnerUnitPrefix), unitSuffix)
		if err := os.WriteFile(filepath.Join(r.Pending, repo), nil, markerPerm); err != nil {
			return err
		}
	}
	return nil
}

func (r Runners) RestartIdle(ctx context.Context) {
	entries, err := os.ReadDir(r.Pending)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		r.Log.Error("ci runner restarts unreadable", "dir", r.Pending, "error", err.Error())
		return
	}
	for _, e := range entries {
		r.restart(ctx, e.Name())
	}
}

func (r Runners) restart(ctx context.Context, repo string) {
	busy, err := r.busy(ctx, repo)
	if err != nil {
		r.Log.Error("ci runner state unknown, restart deferred", "runner", repo, "error", err.Error())
		return
	}
	if busy {
		r.Log.Info("ci runner runs a job, restart deferred", "runner", repo)
		return
	}
	cmd := procexec.Command{Name: procexec.CIRunnerSystemctl, Args: []string{"try-restart", dockerUnitPrefix + repo + unitSuffix, runnerUnitPrefix + repo + unitSuffix}}
	if _, err := r.Exec.Run(ctx, cmd); err != nil {
		r.Log.Error("ci runner restart failed", "runner", repo, "error", err.Error(), "output", procexec.Output(err))
		return
	}
	if err := os.Remove(filepath.Join(r.Pending, repo)); err != nil {
		r.Log.Error("ci runner restart not cleared", "runner", repo, "error", err.Error())
		return
	}
	r.Log.Info("ci runner restarted", "runner", repo)
}

func (r Runners) busy(ctx context.Context, repo string) (bool, error) {
	res, err := r.Exec.Run(ctx, procexec.Command{Name: procexec.CIRunnerSystemctl, Args: []string{"show", "-p", "ControlGroup", "--value", runnerUnitPrefix + repo + unitSuffix}})
	if err != nil {
		return false, err
	}
	cg := strings.TrimSpace(res.Stdout)
	if cg == "" {
		return false, nil
	}
	busy := false
	root := filepath.Join(r.Cgroups, filepath.Clean("/"+cg))
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || busy || d.Name() != cgroupProcs {
			return werr
		}
		busy, werr = r.runsJob(p)
		return werr
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return busy, err
}

func (r Runners) runsJob(procs string) (bool, error) {
	data, err := os.ReadFile(filepath.Clean(procs))
	if err != nil {
		return false, err
	}
	for _, pid := range strings.Fields(string(data)) {
		comm, cerr := os.ReadFile(filepath.Join(r.Proc, filepath.Base(pid), "comm"))
		if cerr == nil && strings.TrimSpace(string(comm)) == jobProcess {
			return true, nil
		}
	}
	return false, nil
}
