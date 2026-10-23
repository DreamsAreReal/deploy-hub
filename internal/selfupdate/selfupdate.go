package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	DefaultBinary    = "/opt/deploy-hub/bin/deploy-hub"
	InstalledCommand = "deploy-hub-installed"
	CandidateCommand = "deploy-hub-candidate"
	VerbVersion      = "version"
	VerbSudoersSync  = "sudoers-sync"
	HubApp           = "deploy-hub"
	candidateSuffix  = ".new"
	previousSuffix   = ".prev"
	stagingSuffix    = ".tmp"
	binaryPerm       = 0o755
	restoredFormat   = "restored=%s"
)

var (
	ErrVersion = errors.New("binary does not report the release commit")
	ErrUpdate  = errors.New("self-update failed, the previous binary was restored")
)

type Updater struct {
	Source  Source
	Binary  string
	Current string
	Exec    procexec.Runner
	Sudo    Sudoers
	Runner  RunnerFiles
	Journal journal.Journal
	Metrics obs.Pusher
	Now     func() time.Time
	Out     *console.Printer
	Log     *slog.Logger
}

func (u Updater) Run(ctx context.Context, tag string) error {
	started := u.Now()
	r, changed, err := u.update(ctx, tag)
	sha7 := journal.NoValue
	if r.Commit != "" {
		sha7 = registry.Sha7(r.Commit)
	}
	u.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{
		{Name: obs.MetricSelfUpdateSuccess, Labels: obs.Labels{}, Value: obs.Bool(err == nil)},
		{Name: obs.MetricSelfUpdateInstalled, Labels: obs.Labels{}, Value: obs.Bool(changed)},
	})
	if !changed && err == nil {
		u.Out.Printf("self-update: %s is up to date\n", sha7)
		return nil
	}
	u.record(sha7, err == nil, started, "")
	if err != nil {
		u.Log.Error("self-update failed", "release", r.Tag, "error", err.Error())
		return err
	}
	u.Out.Printf("self-update: installed %s\n", r.Tag)
	u.Log.Info("self-update installed", "release", r.Tag, "previous", u.Current)
	return nil
}

func (u Updater) record(sha7 string, ok bool, started time.Time, extra string) {
	result := journal.ResultOK
	if !ok {
		result = journal.ResultFail
	}
	e := journal.Entry{App: HubApp, Sha7: sha7, Action: journal.ActionSelfUpdate, Result: result, Seconds: int(u.Now().Sub(started).Seconds()), Extra: extra}
	if err := u.Journal.Append(e); err != nil {
		u.Log.Error("journal append failed", "error", err.Error())
	}
}

func (u Updater) update(ctx context.Context, tag string) (Release, bool, error) {
	r, err := u.Source.Find(ctx, tag)
	if err != nil {
		return Release{}, false, err
	}
	if r.Commit == u.Current {
		changed, cerr := u.keep(ctx)
		return r, changed, cerr
	}
	if gerr := u.Source.Gated(ctx, r.Commit); gerr != nil {
		return r, false, gerr
	}
	bin, err := u.Source.Fetch(ctx, r)
	if err != nil {
		return r, false, err
	}
	return r, true, u.install(ctx, r, bin)
}

func (u Updater) keep(ctx context.Context) (bool, error) {
	if err := u.Sudo.Sync(ctx); err != nil {
		return false, err
	}
	return u.Runner.Sync(ctx)
}

func (u Updater) install(ctx context.Context, r Release, bin []byte) error {
	candidate := u.Binary + candidateSuffix
	_ = os.Remove(candidate)
	if err := writeFile(candidate, bin); err != nil {
		return err
	}
	if err := u.reports(ctx, CandidateCommand, r.Commit); err != nil {
		_ = os.Remove(candidate)
		return err
	}
	if err := copyFile(u.Binary, u.Binary+previousSuffix); err != nil {
		_ = os.Remove(candidate)
		return err
	}
	if err := os.Rename(candidate, u.Binary); err != nil {
		return err
	}
	err := u.reports(ctx, InstalledCommand, r.Commit)
	if err == nil {
		err = u.apply(ctx)
	}
	if err != nil {
		return u.restore(ctx, err)
	}
	return nil
}

func (u Updater) restore(ctx context.Context, cause error) error {
	started := u.Now()
	err := copyFile(u.Binary+previousSuffix, u.Binary)
	if err == nil {
		err = u.apply(context.WithoutCancel(ctx))
	}
	state := journal.ResultOK
	if err != nil {
		state = journal.ResultFail
		u.Log.Error("previous binary not fully restored", "error", err.Error())
	}
	u.record(registry.Sha7(u.Current), err == nil, started, fmt.Sprintf(restoredFormat, state))
	return fmt.Errorf("%w: %w", ErrUpdate, cause)
}

func (u Updater) apply(ctx context.Context) error {
	for _, verb := range []string{VerbSudoersSync, VerbRunnerSync} {
		if _, err := u.Exec.Run(ctx, procexec.Command{Name: InstalledCommand, Args: []string{verb}}); err != nil {
			return fmt.Errorf("%s: %w: %s", verb, err, procexec.Output(err))
		}
	}
	return nil
}

func (u Updater) reports(ctx context.Context, command, commit string) error {
	res, err := u.Exec.Run(ctx, procexec.Command{Name: command, Args: []string{VerbVersion}})
	if err != nil {
		return fmt.Errorf("%w: %w: %s", ErrVersion, err, procexec.Output(err))
	}
	if got := strings.TrimSpace(res.Stdout); got != commit {
		return fmt.Errorf("%w: got %q want %s", ErrVersion, got, commit)
	}
	return nil
}

func writeFile(path string, data []byte) error {
	return writeMode(path, data, binaryPerm)
}

func writeMode(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	return errors.Join(werr, f.Chmod(perm), f.Sync(), f.Close())
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(filepath.Clean(src))
	if err != nil {
		return err
	}
	staged := dst + stagingSuffix
	_ = os.Remove(staged)
	if err := writeFile(staged, data); err != nil {
		return err
	}
	return os.Rename(staged, dst)
}
