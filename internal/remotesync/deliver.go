package remotesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	backupFile    = "/tmp/dh-backup.tar.gz"
	backupPerm    = 0o600
	backupDirPerm = 0o700
	keepBackups   = 10
	stampLayout   = "20060102T150405Z"
	overlayMount  = "/overlay"
	tmpMount      = "/tmp"
	kib           = 1024
	overlaySpare  = 512 * kib
	tmpSpare      = 4 << 20
	dfFields      = 6
	pendingPrefix = "pending:"
	healthyMark   = "(healthy)"
	confirmedWord = "confirmed"
)

var (
	ErrBusy       = errors.New("another change is pending on the target")
	ErrNoSpace    = errors.New("not enough free space on the target")
	ErrBackup     = errors.New("backup of the target failed")
	ErrApply      = errors.New("the target refused the release")
	ErrRolledBack = errors.New("the target rolled the release back")
	gzipMagic     = []byte{0x1f, 0x8b}
)

func (s Courier) deliver(ctx context.Context, p parcel) (Outcome, error) {
	host, err := s.reach(ctx, p.conf)
	out := Outcome{Host: host}
	if err != nil {
		return out, err
	}
	steps := []func(context.Context, parcel, string) error{s.preflight, s.backup, s.upload}
	for _, step := range steps {
		if err = step(ctx, p, host); err != nil {
			return out, err
		}
	}
	text, err := s.on(ctx, p.conf, host, p.engine+" --release "+p.dir, "")
	if err != nil {
		out.Result = lastLine(text)
		return out, fmt.Errorf("%w: %s", ErrApply, out.Result)
	}
	out.Result, err = s.settle(ctx, p, host)
	if !errors.Is(err, ErrUnreachable) {
		s.cleanup(ctx, p)
	}
	return out, err
}

func (s Courier) preflight(ctx context.Context, p parcel, host string) error {
	text, err := s.on(ctx, p.conf, host, "[ ! -x "+InstalledEngine+" ] || "+InstalledEngine+" status; df -k "+overlayMount+" "+tmpMount, "")
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoSpace, lastLine(text))
	}
	if strings.Contains(text, "\n"+pendingPrefix) || strings.HasPrefix(text, pendingPrefix) {
		return fmt.Errorf("%w: %s", ErrBusy, firstLine(text))
	}
	free := freeSpace(text)
	if free[overlayMount] < 2*p.size+overlaySpare || free[tmpMount] < tmpSpare {
		return fmt.Errorf("%w: overlay %d B, tmp %d B free, release %d B", ErrNoSpace, free[overlayMount], free[tmpMount], p.size)
	}
	return nil
}

func freeSpace(df string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(df, "\n") {
		f := strings.Fields(line)
		if len(f) != dfFields {
			continue
		}
		if n, err := strconv.Atoi(f[3]); err == nil {
			out[f[5]] = n * kib
		}
	}
	return out
}

func (s Courier) backup(ctx context.Context, p parcel, host string) error {
	text, err := s.on(ctx, p.conf, host, "sysupgrade -b "+backupFile+" >/dev/null && cat "+backupFile+"; rm -f "+backupFile, "")
	if err != nil || !bytes.HasPrefix([]byte(text), gzipMagic) {
		return fmt.Errorf("%w: %s", ErrBackup, lastLine(text))
	}
	dir := filepath.Join(s.Backups, p.target)
	if err := os.MkdirAll(dir, backupDirPerm); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.tar.gz", s.Now().UTC().Format(stampLayout), p.sha7)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), backupPerm); err != nil {
		return err
	}
	return prune(dir)
}

func prune(dir string) error {
	names, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil || len(names) <= keepBackups {
		return err
	}
	slices.Sort(names)
	var errs []error
	for _, n := range names[:len(names)-keepBackups] {
		errs = append(errs, os.Remove(n))
	}
	return errors.Join(errs...)
}

func (s Courier) upload(ctx context.Context, p parcel, host string) error {
	text, err := s.on(ctx, p.conf, host, "rm -rf "+p.dir+" && mkdir -p "+p.dir+" && tar -x -C "+p.dir, string(p.archive))
	if err != nil {
		return fmt.Errorf("upload to %s failed: %s", host, lastLine(text))
	}
	return nil
}

func (s Courier) settle(ctx context.Context, p parcel, host string) (string, error) {
	for waited := time.Duration(0); waited < s.HealthWait; waited += s.Poll {
		s.Sleep(s.Poll)
		text, err := s.on(ctx, p.conf, host, p.engine+" status", "")
		switch {
		case err != nil:
			continue
		case strings.Contains(text, healthyMark):
			return s.confirm(ctx, p)
		case !strings.HasPrefix(text, pendingPrefix):
			return lastLine(text), fmt.Errorf("%w: %s", ErrRolledBack, lastLine(text))
		}
	}
	_, _ = s.on(ctx, p.conf, host, p.engine+" rollback", "")
	return s.verdict(ctx, p)
}

func (s Courier) confirm(ctx context.Context, p parcel) (string, error) {
	host, err := s.reach(ctx, p.conf)
	if err == nil {
		text, cerr := s.on(ctx, p.conf, host, p.engine+" confirm", "")
		if cerr == nil && lastLine(text) == confirmedWord {
			return confirmedWord, nil
		}
	}
	return s.verdict(ctx, p)
}

func (s Courier) verdict(ctx context.Context, p parcel) (string, error) {
	for waited := time.Duration(0); waited < s.VerdictWait; waited += s.Poll {
		s.Sleep(s.Poll)
		host, err := s.reach(ctx, p.conf)
		if err != nil {
			continue
		}
		text, err := s.on(ctx, p.conf, host, p.engine+" status", "")
		if err == nil && !strings.HasPrefix(text, pendingPrefix) {
			result := lastLine(text)
			if strings.HasPrefix(result, confirmedWord) {
				return result, nil
			}
			return result, fmt.Errorf("%w: %s", ErrRolledBack, result)
		}
	}
	return "no verdict", fmt.Errorf("%w: no verdict read in %s", ErrUnreachable, s.VerdictWait)
}

func (s Courier) cleanup(ctx context.Context, p parcel) {
	host, err := s.reach(ctx, p.conf)
	if err == nil {
		_, err = s.on(ctx, p.conf, host, "rm -rf "+p.dir, "")
	}
	if err != nil {
		s.Log.Warn("release directory left on the target", "dir", p.dir, "error", err.Error())
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
