package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DefaultUnitDir   = "/etc/systemd/system"
	VerbUnitSync     = "systemd-sync"
	AnalyzeCommand   = "systemd-analyze"
	SystemctlCommand = "systemctl"
	unitPerm         = 0o644
	unitTempPrefix   = "."
	unitTempSuffix   = ".deploy-hub-new"
	timerSuffix      = ".timer"
	verbReload       = "daemon-reload"
)

var ErrUnitActivate = errors.New("systemd units not activated, the previous units were restored")

type UnitFiles struct {
	Files fs.FS
	Dir   string
	Exec  procexec.Runner
}

type unitChange struct {
	path    string
	want    []byte
	old     []byte
	existed bool
}

func (u UnitFiles) Sync(ctx context.Context) (bool, error) {
	changes, err := u.plan()
	if err != nil || len(changes) == 0 {
		return false, err
	}
	err = u.write(changes)
	if err == nil {
		err = u.activate(ctx, changes)
	}
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrUnitActivate, errors.Join(err, u.restore(context.WithoutCancel(ctx), changes)))
	}
	return true, nil
}

func (u UnitFiles) plan() ([]unitChange, error) {
	entries, err := fs.ReadDir(u.Files, ".")
	var changes []unitChange
	for _, e := range entries {
		c := unitChange{path: filepath.Join(u.Dir, e.Name())}
		same, cerr := c.read(u.Files, e.Name())
		err = errors.Join(err, cerr)
		if !same {
			changes = append(changes, c)
		}
	}
	return changes, err
}

func (c *unitChange) read(files fs.FS, name string) (bool, error) {
	want, err := fs.ReadFile(files, name)
	c.want = want
	info, lerr := os.Lstat(c.path)
	if errors.Is(lerr, fs.ErrNotExist) {
		return false, err
	}
	if lerr == nil {
		c.existed = true
		c.old, lerr = os.ReadFile(filepath.Clean(c.path))
	}
	err = errors.Join(err, lerr)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm() == unitPerm && bytes.Equal(c.old, c.want), err
}

func (u UnitFiles) write(changes []unitChange) error {
	for _, c := range changes {
		if err := writeUnit(c.path, c.want); err != nil {
			return err
		}
	}
	return nil
}

func writeUnit(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), unitTempPrefix+filepath.Base(path)+unitTempSuffix)
	_ = os.Remove(tmp)
	err := writeMode(tmp, data, unitPerm)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (u UnitFiles) activate(ctx context.Context, changes []unitChange) error {
	paths := make([]string, 0, len(changes))
	var timers []string
	for _, c := range changes {
		paths = append(paths, c.path)
		if strings.HasSuffix(c.path, timerSuffix) {
			timers = append(timers, filepath.Base(c.path))
		}
	}
	steps := [][]string{append([]string{AnalyzeCommand, "verify"}, paths...), {SystemctlCommand, verbReload}}
	if len(timers) > 0 {
		steps = append(steps, append([]string{SystemctlCommand, "enable", "--now"}, timers...))
	}
	return u.run(ctx, steps)
}

func (u UnitFiles) run(ctx context.Context, steps [][]string) error {
	for _, st := range steps {
		if _, err := u.Exec.Run(ctx, procexec.Command{Name: st[0], Args: st[1:]}); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(st, " "), err, procexec.Output(err))
		}
	}
	return nil
}

func (u UnitFiles) restore(ctx context.Context, changes []unitChange) error {
	var errs []error
	for _, c := range changes {
		if c.existed {
			errs = append(errs, writeUnit(c.path, c.old))
			continue
		}
		if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, u.run(ctx, [][]string{{SystemctlCommand, verbReload}}))
	return errors.Join(errs...)
}
