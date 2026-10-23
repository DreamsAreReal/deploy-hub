package mutation

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const workDir = "/w"

func fixedBytes(n int64, err error) func(string) (int64, error) {
	return func(string) (int64, error) { return n, err }
}

func TestDiskGuardLimits(t *testing.T) {
	broken := errors.New("statfs failed")
	cases := []struct {
		name  string
		guard diskGuard
		want  string
	}{
		{"within both limits", diskGuard{limit: 20 * gib, minFree: 10 * gib, size: fixedBytes(gib, nil), free: fixedBytes(50*gib, nil)}, ""},
		{"work directory too big", diskGuard{dir: workDir, limit: 20 * gib, minFree: 10 * gib, size: fixedBytes(21*gib, nil), free: fixedBytes(50*gib, nil)}, "/w holds 21504 MiB, the policy allows 20 GiB (mutation_disk_gib)"},
		{"disk almost full", diskGuard{dir: workDir, limit: 20 * gib, minFree: 10 * gib, size: fixedBytes(gib, nil), free: fixedBytes(9*gib, nil)}, "/w has 9216 MiB free, the policy keeps 10 GiB free (mutation_free_gib)"},
		{"size unknown", diskGuard{limit: gib, size: fixedBytes(0, broken)}, broken.Error()},
		{"free space unknown", diskGuard{minFree: gib, free: fixedBytes(0, broken)}, broken.Error()},
		{"no limits", diskGuard{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.guard.check()
			if c.want == "" {
				testutil.NoErr(t, err)
				return
			}
			testutil.True(t, errors.Is(err, ErrDisk), "the stop names the disk")
			testutil.Contains(t, err.Error(), c.want)
		})
	}
}

func TestTreeSizeCountsHardLinksOnce(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "base", "a"), strings.Repeat("x", 1000))
	testutil.Write(t, filepath.Join(dir, "b"), strings.Repeat("y", 24))
	testutil.NoErr(t, os.Link(filepath.Join(dir, "base", "a"), filepath.Join(dir, "a-link")))
	size, err := treeSize(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, size, int64(1024))
	size, err = treeSize(filepath.Join(dir, "missing"))
	testutil.NoErr(t, err)
	testutil.Equal(t, size, int64(0))
	free, err := freeBytes(dir)
	testutil.NoErr(t, err)
	testutil.True(t, free > 0, "statfs reports free space")
	_, err = freeBytes(filepath.Join(dir, "missing"))
	testutil.Fails(t, err)
}

func TestDiskGuardStopsTheRunWithItsReason(t *testing.T) {
	g := diskGuard{dir: workDir, limit: gib, size: fixedBytes(2*gib, nil)}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go g.watch(ctx, cancel, time.Millisecond)
	<-ctx.Done()
	testutil.True(t, errors.Is(context.Cause(ctx), ErrDisk), "the cause is the disk limit")
	quiet := diskGuard{limit: gib, size: fixedBytes(0, nil)}
	qctx, qcancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	go func() { quiet.watch(qctx, qcancel, time.Millisecond); close(done) }()
	time.Sleep(5 * time.Millisecond)
	qcancel(nil)
	<-done
	testutil.Equal(t, context.Cause(qctx), context.Canceled)
}

func TestRunDoesNotStartWithoutFreeSpace(t *testing.T) {
	ws, fg := cachedSetup(t)
	ws.Policy.Limits.MutationFree = 1 << 20
	_, err := Run(context.Background(), ws)
	testutil.True(t, errors.Is(err, ErrIncomplete) && errors.Is(err, ErrDisk), "the run stops before it starts")
	testutil.Contains(t, err.Error(), "mutation_free_gib")
	testutil.Equal(t, len(fg.overlays), 0)
}

func TestDiskStopKeepsTheFinishedVerdicts(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	jobs := []Job{
		{Mutant: Mutant{File: fileLib}, Run: func(context.Context, int) Status { return StatusKilled }},
		{Mutant: Mutant{File: fileLib}, Run: func(context.Context, int) Status {
			cancel(errors.Join(ErrDisk, errors.New("work directory too big")))
			return StatusKilled
		}},
	}
	rep, err := Execute(ctx, gate.Workspace{Src: "/", Log: slog.New(slog.DiscardHandler)}, jobs, 1)
	testutil.True(t, errors.Is(err, ErrIncomplete) && errors.Is(err, ErrDisk), "the disk stop is reported")
	testutil.Equal(t, rep.Entries[0].Status, StatusKilled)
	testutil.Equal(t, rep.Entries[1].Status, StatusErrored)
}

func TestBlocksNeverOverflow(t *testing.T) {
	testutil.Equal(t, blocks(7), int64(7))
	testutil.Equal(t, blocks(^uint64(0)), int64(math.MaxInt64>>maxBlockShift))
}
