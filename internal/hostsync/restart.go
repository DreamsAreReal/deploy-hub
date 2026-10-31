package hostsync

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/lock"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	DefaultRestarts = "/var/lib/deploy-hub/host-restart"
	stateSuffix     = ".json"
	lockSuffix      = ".lock"
	unknownPrev     = "unknown"
	stateLockWait   = 10 * time.Minute
	stateLockPoll   = time.Second
)

type RestartState struct {
	Installed string   `json:"installed,omitempty"`
	Pending   *Pending `json:"pending,omitempty"`
}

type Pending struct {
	Tag     string    `json:"tag"`
	PrevTag string    `json:"prev_tag,omitempty"`
	Since   time.Time `json:"since"`
}

func PrevName(dst, tag string) string {
	label := unknownPrev
	if tag != "" {
		label = registry.Sha7(tag)
	}
	return dst + policy.PrevInfix + label
}

func statePath(dir, app string) string {
	return filepath.Join(dir, app+stateSuffix)
}

func LoadState(dir, app string) (RestartState, error) {
	var st RestartState
	raw, err := readLimited(statePath(dir, app), MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	return st, err
}

func SaveState(dir, app string, st RestartState) error {
	if err := os.MkdirAll(dir, backupPerm); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := statePath(dir, app)
	tmp := path + tempSuffix
	_ = os.Remove(tmp)
	if err := writeBody(tmp, bytesBody(raw)); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LockState(ctx context.Context, dir, app string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(dir, backupPerm); err != nil {
		return nil, err
	}
	l := lock.File{Path: filepath.Join(dir, app+lockSuffix), Wait: wait, Poll: stateLockPoll, Sleep: health.Sleep}
	return l.Acquire(ctx, func() {})
}

func (s Syncer) restartState(ctx context.Context, t target) (RestartState, func(), error) {
	if t.spec.Restart == nil {
		return RestartState{}, func() {}, nil
	}
	release, err := LockState(ctx, s.Restarts, t.app, stateLockWait)
	if err != nil {
		return RestartState{}, nil, err
	}
	st, err := LoadState(s.Restarts, t.app)
	if err != nil {
		release()
		return RestartState{}, nil, err
	}
	return st, release, nil
}

func (s Syncer) keepPrevious(t target, st RestartState, changed []plannedFile) error {
	if t.spec.Restart == nil || st.Pending != nil {
		return nil
	}
	for _, p := range changed {
		if !p.spec.KeepPrev || !p.existed {
			continue
		}
		if err := s.keepOne(p, PrevName(p.path, st.Installed)); err != nil {
			return err
		}
	}
	return nil
}

func (s Syncer) keepOne(p plannedFile, prev string) error {
	if err := s.writeFile(prev, p.old); err != nil {
		return err
	}
	s.Log.Info("previous host file kept", "path", p.spec.Dst, "copy", filepath.Base(prev))
	return inParent(p.path, func(r *os.Root, name string) error {
		return prunePrevious(r, name+policy.PrevInfix, filepath.Base(prev))
	})
}

func prunePrevious(r *os.Root, prefix, keep string) error {
	entries, err := fs.ReadDir(r.FS(), ".")
	errs := []error{err}
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, prefix) && n != keep {
			errs = append(errs, r.Remove(n))
		}
	}
	return errors.Join(errs...)
}

func (s Syncer) markPending(t target, st RestartState, changed int) error {
	if t.spec.Restart == nil || changed == 0 {
		return nil
	}
	if st.Pending == nil {
		st.Pending = &Pending{PrevTag: st.Installed, Since: s.Now().UTC()}
	}
	st.Pending.Tag = t.tag
	s.Log.Info("host restart pending", "app", t.app, "tag", t.tag, "unit", t.spec.Restart.Unit, "previous", st.Pending.PrevTag)
	return SaveState(s.Restarts, t.app, st)
}
