package procexec

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	memoryPoll     = 200 * time.Millisecond
	statFile       = "stat"
	meminfoFile    = "meminfo"
	memTotalField  = "MemTotal:"
	kilobyte       = 1024
	kilobyteUnit   = "kB"
	statPgrpField  = 2
	statRSSField   = 21
	statMinFields  = statRSSField + 1
	statNameClosed = ')'
)

var (
	ErrMemoryLimit = errors.New("memory limit exceeded")
	procDir        = "/proc"
	groupRSS       = procGroupRSS
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func guard(pgid int, limit int64, over *atomic.Bool, stop <-chan struct{}) {
	tick := time.NewTicker(memoryPoll)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if groupRSS(pgid) > limit {
				over.Store(true)
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				return
			}
		}
	}
}

func procGroupRSS(pgid int) int64 {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	group := strconv.Itoa(pgid)
	var pages int64
	for _, e := range entries {
		data, rerr := os.ReadFile(filepath.Join(procDir, e.Name(), statFile))
		if rerr == nil {
			pages += statRSS(data, group)
		}
	}
	return pages * int64(os.Getpagesize())
}

func statRSS(stat []byte, group string) int64 {
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, statNameClosed)+1:]))
	if len(fields) < statMinFields || fields[statPgrpField] != group {
		return 0
	}
	pages, _ := strconv.ParseInt(fields[statRSSField], 10, 64)
	return pages
}

func TotalMemory() int64 {
	total := memTotal()
	if limit, ok := cgroupLimit(memoryMaxFile, parseMemoryMax); ok && (total == 0 || limit < total) {
		return limit
	}
	return total
}

func memTotal() int64 {
	data, err := os.ReadFile(filepath.Join(procDir, meminfoFile))
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, memTotalField); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), kilobyteUnit)), 10, 64)
			return kb * kilobyte
		}
	}
	return 0
}
