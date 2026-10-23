package mutation

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestDiskGuardLimitsAreInclusive(t *testing.T) {
	ws := gate.Workspace{Out: "/w", Policy: policy.Policy{Limits: policy.Limits{MutationDisk: 3, MutationFree: 2}}}
	g := newDiskGuard(ws)
	testutil.Equal(t, g.limit, 3*int64(1<<30))
	testutil.Equal(t, g.minFree, 2*int64(1<<30))
	testutil.Equal(t, g.dir, "/w")
	g.size, g.free = fixedBytes(g.limit, nil), fixedBytes(g.minFree, nil)
	testutil.NoErr(t, g.check())
	g.size = fixedBytes(g.limit+1, nil)
	testutil.True(t, errors.Is(g.check(), ErrDisk), "one byte over the limit stops the run")
	g.size, g.free = fixedBytes(g.limit, nil), fixedBytes(g.minFree-1, nil)
	testutil.True(t, errors.Is(g.check(), ErrDisk), "one byte under the free space stops the run")
}

func TestFreeBytesMatchesStatfs(t *testing.T) {
	dir := t.TempDir()
	var st syscall.Statfs_t
	testutil.NoErr(t, syscall.Statfs(dir, &st))
	free, err := freeBytes(dir)
	testutil.NoErr(t, err)
	want := float64(st.Bavail) * float64(st.Bsize)
	testutil.True(t, float64(free) > want-(1<<26) && float64(free) < want+(1<<26), "free space comes from statfs")
	edge := uint64(math.MaxInt64 >> maxBlockShift)
	testutil.Equal(t, blocks(edge), int64(edge))
	testutil.Equal(t, blocks(edge+1), int64(edge))
}

func TestTreeSizeReportsUnreadableDirectories(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	testutil.Write(t, filepath.Join(locked, "f"), "x")
	info, err := os.Stat(locked)
	testutil.NoErr(t, err)
	testutil.NoErr(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, info.Mode().Perm()) })
	_, err = treeSize(dir)
	testutil.True(t, errors.Is(err, fs.ErrPermission), "an unreadable directory is an error, not zero bytes")
	testutil.NoErr(t, vanished(fs.ErrNotExist))
	testutil.True(t, errors.Is(vanished(fs.ErrPermission), fs.ErrPermission), "only vanished files are skipped")
}

func TestWorkerCacheBudgetIsInclusive(t *testing.T) {
	base := filepath.Join(t.TempDir(), cacheDir)
	testutil.Write(t, filepath.Join(base, "ab", "ab12-a"), "action")
	caches, err := (&engine{cacheDir: base}).workerCaches(1)
	testutil.NoErr(t, err)
	w := caches[0]
	w.keep = 5
	testutil.Write(t, filepath.Join(w.dir, "cd", "cd56-d"), "12345")
	testutil.NoErr(t, w.trim())
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.dir, "cd", "cd56-d")), "12345")
	testutil.Write(t, filepath.Join(w.dir, "cd", "cd57-d"), "6")
	testutil.NoErr(t, w.trim())
	testutil.Missing(t, filepath.Join(w.dir, "cd", "cd56-d"))
	entries, err := os.ReadDir(filepath.Join(base, "ab"))
	testutil.NoErr(t, err)
	testutil.Equal(t, entrySize(entries[0]), int64(len("action")))
}

func TestBuildCacheOverTheDiskLimitStopsTheRunBeforeAnyMutant(t *testing.T) {
	for _, phase := range []string{"baseline", "index"} {
		t.Run(phase, func(t *testing.T) {
			ws, fg := cachedSetup(t)
			ws.Policy.Limits.MutationDisk = 1
			fg.grow = phase
			_, err := Run(context.Background(), ws)
			testutil.True(t, errors.Is(err, ErrDisk), "a build cache over the limit stops the run")
			testutil.Contains(t, err.Error(), "the policy allows 1 GiB (mutation_disk_gib)")
			testutil.Equal(t, len(fg.overlays), 0)
			testutil.Missing(t, filepath.Join(ws.Out, cacheDir))
		})
	}
}
