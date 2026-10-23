package mutation

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"syscall"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	gib           = int64(1) << 30
	mib           = int64(1) << 20
	diskEvery     = 15 * time.Second
	maxBlockShift = 20
	reasonDisk    = "%s holds %d MiB, the policy allows %d GiB (mutation_disk_gib)"
	reasonFree    = "%s has %d MiB free, the policy keeps %d GiB free (mutation_free_gib)"
)

var ErrDisk = errors.New("mutation testing stopped to protect the disk")

type diskGuard struct {
	dir     string
	limit   int64
	minFree int64
	size    func(string) (int64, error)
	free    func(string) (int64, error)
}

func newDiskGuard(ws gate.Workspace) diskGuard {
	l := ws.Policy.Limits
	return diskGuard{dir: ws.Out, limit: l.MutationDisk * gib, minFree: l.MutationFree * gib, size: treeSize, free: freeBytes}
}

func (g diskGuard) check() error {
	return errors.Join(g.checkSize(), g.checkFree())
}

func (g diskGuard) checkSize() error {
	if g.limit <= 0 {
		return nil
	}
	used, err := g.size(g.dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDisk, err)
	}
	if used > g.limit {
		return fmt.Errorf("%w: "+reasonDisk, ErrDisk, g.dir, used/mib, g.limit/gib)
	}
	return nil
}

func (g diskGuard) checkFree() error {
	if g.minFree <= 0 {
		return nil
	}
	free, err := g.free(g.dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDisk, err)
	}
	if free < g.minFree {
		return fmt.Errorf("%w: "+reasonFree, ErrDisk, g.dir, free/mib, g.minFree/gib)
	}
	return nil
}

func (g diskGuard) watch(ctx context.Context, cancel context.CancelCauseFunc, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := g.check(); err != nil {
				cancel(err)
				return
			}
		}
	}
}

type tree struct {
	seen  map[uint64]bool
	total int64
}

func treeSize(dir string) (int64, error) {
	t := &tree{seen: map[uint64]bool{}}
	err := filepath.WalkDir(dir, t.visit)
	return t.total, err
}

func (t *tree) visit(_ string, e fs.DirEntry, err error) error {
	if err != nil || e.IsDir() {
		return vanished(err)
	}
	info, err := e.Info()
	if err != nil {
		return vanished(err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if t.seen[st.Ino] {
			return nil
		}
		t.seen[st.Ino] = true
	}
	t.total += info.Size()
	return nil
}

func vanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return blocks(st.Bavail) * st.Bsize, nil
}

func blocks(n uint64) int64 {
	if n > math.MaxInt64>>maxBlockShift {
		return math.MaxInt64 >> maxBlockShift
	}
	return int64(n)
}
