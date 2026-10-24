package hostsync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	Caddyfile     = "/etc/caddy/Caddyfile"
	caddyService  = "caddy"
	daemonReload  = "daemon-reload"
	verbReload    = "reload"
	caddyAdapter  = "caddyfile"
	tempPrefix    = "."
	tempSuffix    = ".deploy-hub-new"
	tempPerm      = 0o600
	backupPerm    = 0o700
	keepBackups   = 5
	stampLayout   = "20060102T150405Z"
	manifestName  = "manifest"
	manifestEntry = "%d %o %d %d %s\n"
	absentEntry   = "- - - - %s\n"
	restoredExtra = "files=%d"
)

var ErrActivate = errors.New("host activation failed, previous files restored")

func (s Syncer) install(ctx context.Context, t target, files []plannedFile) (int, error) {
	changed := slices.DeleteFunc(slices.Clone(files), func(p plannedFile) bool { return !p.changed })
	if err := s.backup(t, changed); err != nil {
		return 0, err
	}
	if err := s.ensureDirs(t.spec.Dirs); err != nil {
		return 0, err
	}
	err := s.write(changed)
	if err == nil {
		err = s.activate(ctx, t.spec, changed)
	}
	if err != nil {
		s.restore(ctx, t, changed)
		return len(changed), fmt.Errorf("%w: %w", ErrActivate, err)
	}
	return len(changed), nil
}

func (s Syncer) backup(t target, changed []plannedFile) error {
	if len(changed) == 0 {
		return nil
	}
	root := filepath.Join(s.Backups, t.app)
	dir := filepath.Join(root, s.Now().UTC().Format(stampLayout)+"-"+registry.Sha7(t.tag))
	if err := os.MkdirAll(dir, backupPerm); err != nil {
		return err
	}
	var manifest strings.Builder
	for i, p := range changed {
		if !p.existed {
			fmt.Fprintf(&manifest, absentEntry, p.spec.Dst)
			continue
		}
		saved := filepath.Join(dir, strconv.Itoa(i))
		_ = os.Remove(saved)
		if err := writeBody(saved, p.old.body); err != nil {
			return err
		}
		changed[i].old.body = body{path: saved, limit: MaxBinarySize, sum: p.old.body.sum}
		fmt.Fprintf(&manifest, manifestEntry, i, p.old.mode, p.old.uid, p.old.gid, p.spec.Dst)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest.String()), tempPerm); err != nil {
		return err
	}
	return pruneBackups(root)
}

func pruneBackups(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var errs []error
	for i := range len(entries) - keepBackups {
		errs = append(errs, os.RemoveAll(filepath.Join(root, entries[i].Name())))
	}
	return errors.Join(errs...)
}

func (s Syncer) ensureDirs(dirs []policy.HostDir) error {
	for _, d := range dirs {
		mode, _ := policy.ParseMode(d.Mode)
		uid, gid, err := s.Accounts(d.Owner, d.Group)
		if err != nil {
			return err
		}
		p := s.hostPath(d.Path)
		if err := os.MkdirAll(p, fs.FileMode(mode)); err != nil {
			return err
		}
		if err := realDir(p); err != nil {
			return err
		}
		if err := errors.Join(s.Chown(p, uid, gid), os.Chmod(p, fs.FileMode(mode))); err != nil {
			return err
		}
	}
	return nil
}

func (s Syncer) write(changed []plannedFile) error {
	for _, p := range changed {
		if err := s.writeFile(p.path, p.want); err != nil {
			return err
		}
		s.Log.Info("host file installed", "path", p.spec.Dst, "mode", fmt.Sprintf("%o", p.want.mode), "uid", p.want.uid, "gid", p.want.gid)
	}
	return nil
}

func (s Syncer) writeFile(path string, want snapshot) error {
	tmp := filepath.Join(filepath.Dir(path), tempPrefix+filepath.Base(path)+tempSuffix)
	_ = os.Remove(tmp)
	err := writeBody(tmp, want.body)
	if err == nil {
		err = errors.Join(s.Chown(tmp, want.uid, want.gid), os.Chmod(tmp, want.mode))
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (s Syncer) activate(ctx context.Context, spec policy.Host, changed []plannedFile) error {
	units, caddy := touched(changed)
	steps := [][]string{}
	if len(units) > 0 {
		steps = append(steps, append([]string{AnalyzeBinary, "verify"}, units...), []string{SystemctlBinary, daemonReload})
	}
	if caddy {
		steps = append(steps, []string{CaddyBinary, "validate", "--config", s.hostPath(Caddyfile), "--adapter", caddyAdapter}, []string{SystemctlBinary, verbReload, caddyService})
	}
	if len(spec.Timers) > 0 {
		steps = append(steps, append([]string{SystemctlBinary, "enable", "--now"}, spec.Timers...))
	}
	for _, c := range spec.Checks {
		steps = append(steps, []string{SystemctlBinary, "start", c})
	}
	return s.run(ctx, steps)
}

func touched(changed []plannedFile) ([]string, bool) {
	var units []string
	caddy := false
	for _, p := range changed {
		units = appendIf(units, strings.HasPrefix(p.spec.Dst, policy.UnitDir), p.path)
		caddy = caddy || strings.HasPrefix(p.spec.Dst, policy.CaddySiteDir)
	}
	return units, caddy
}

func appendIf(list []string, ok bool, v string) []string {
	if ok {
		return append(list, v)
	}
	return list
}

func (s Syncer) run(ctx context.Context, steps [][]string) error {
	for _, st := range steps {
		if _, err := s.Exec.Run(ctx, procexec.Command{Name: st[0], Args: st[1:]}); err != nil {
			s.Log.Error("host activation step failed", "step", strings.Join(st, " "), "output", procexec.Output(err))
			return fmt.Errorf("%s: %w: %s", strings.Join(st, " "), err, procexec.Output(err))
		}
		s.Log.Info("host activation step done", "step", strings.Join(st, " "))
	}
	return nil
}

func (s Syncer) restore(ctx context.Context, t target, changed []plannedFile) {
	started := s.Now()
	var errs []error
	for _, p := range changed {
		if !p.existed {
			errs = append(errs, ignoreMissing(os.Remove(p.path)))
			continue
		}
		errs = append(errs, s.writeFile(p.path, p.old))
	}
	units, caddy := touched(changed)
	var steps [][]string
	if len(units) > 0 {
		steps = append(steps, []string{SystemctlBinary, daemonReload})
	}
	if caddy {
		steps = append(steps, []string{SystemctlBinary, verbReload, caddyService})
	}
	errs = append(errs, s.run(context.WithoutCancel(ctx), steps))
	err := errors.Join(errs...)
	s.record(t.app, t.tag, journal.ActionHostBack, err == nil, started, fmt.Sprintf(restoredExtra, len(changed)))
	if err != nil {
		s.Log.Error("host files not fully restored", "app", t.app, "error", err.Error())
	}
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
