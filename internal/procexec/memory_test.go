package procexec

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	groupID      = 4242
	limitBytes   = 1 << 30
	waitingShell = "sleep 30 & wait"
)

func stat(pgrp, rss int) string {
	return strconv.Itoa(pgrp) + " (name with) spaces) S 1 " + strconv.Itoa(pgrp) + " 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 1000 " + strconv.Itoa(rss) + " 0\n"
}

func fakeProc(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	previous := procDir
	procDir = dir
	t.Cleanup(func() { procDir = previous })
}

func TestGroupRSSSumsTheProcessGroup(t *testing.T) {
	fakeProc(t, map[string]string{
		"10/stat":     stat(groupID, 100),
		"11/stat":     stat(groupID, 50),
		"12/stat":     stat(groupID+1, 999),
		"13/stat":     "13 (short) S 1",
		"self/status": "x",
	})
	testutil.Equal(t, procGroupRSS(groupID), int64(150*os.Getpagesize()))
	procDir = filepath.Join(t.TempDir(), "missing")
	testutil.Equal(t, procGroupRSS(groupID), int64(0))
}

func TestTotalMemory(t *testing.T) {
	fakeProc(t, map[string]string{meminfoFile: "MemFree:  10 kB\nMemTotal:       16384 kB\n"})
	testutil.Equal(t, TotalMemory(), int64(16384*kilobyte))
	fakeProc(t, map[string]string{meminfoFile: "MemFree:  10 kB\n"})
	testutil.Equal(t, TotalMemory(), int64(0))
	procDir = filepath.Join(t.TempDir(), "missing")
	testutil.Equal(t, TotalMemory(), int64(0))
}

func withGroupRSS(t *testing.T, rss int64) {
	t.Helper()
	previous := groupRSS
	groupRSS = func(int) int64 { return rss }
	t.Cleanup(func() { groupRSS = previous })
}

func TestMemoryLimitKillsTheGroup(t *testing.T) {
	withGroupRSS(t, limitBytes+1)
	started := time.Now()
	_, err := testOS().Run(context.Background(), Command{Name: shell, Args: []string{shellFlag, waitingShell}, MemoryLimit: limitBytes})
	testutil.Is(t, err, ErrMemoryLimit)
	testutil.True(t, time.Since(started) < 10*time.Second, "the group is killed at once")
}

func TestMemoryLimitAllowsQuietCommands(t *testing.T) {
	withGroupRSS(t, 1)
	res, err := testOS().Run(context.Background(), Command{Name: shell, Args: []string{shellFlag, "sleep 0.5; echo done"}, MemoryLimit: limitBytes})
	testutil.NoErr(t, err)
	testutil.Equal(t, res.Stdout, "done\n")
	_, err = testOS().Run(context.Background(), Command{Name: missing, MemoryLimit: limitBytes})
	testutil.Fails(t, err)
}

func TestCancelKillsTheWholeGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := testOS().Run(ctx, Command{Name: shell, Args: []string{shellFlag, waitingShell}, MemoryLimit: limitBytes})
	testutil.Fails(t, err)
	testutil.True(t, time.Since(started) < waitDelay, "children die with the group, no wait delay")
}
