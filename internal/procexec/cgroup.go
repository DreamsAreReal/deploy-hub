package procexec

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	cgroupFile     = "self/cgroup"
	cgroupV2Prefix = "0::"
	memoryMaxFile  = "memory.max"
	cpuMaxFile     = "cpu.max"
	cpuMaxFields   = 2
)

var cgroupDir = "/sys/fs/cgroup"

func CPUs() int {
	cpus := int64(runtime.NumCPU())
	if quota, ok := cgroupLimit(cpuMaxFile, parseCPUMax); ok {
		cpus = min(cpus, quota)
	}
	return int(cpus)
}

func cgroupLimit(name string, parse func(string) (int64, bool)) (int64, bool) {
	var (
		limit int64
		found bool
	)
	for _, dir := range cgroupChain() {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		if err != nil {
			continue
		}
		if v, ok := parse(strings.TrimSpace(string(data))); ok && (!found || v < limit) {
			limit, found = v, true
		}
	}
	return limit, found
}

func cgroupChain() []string {
	data, err := os.ReadFile(filepath.Join(procDir, cgroupFile))
	if err != nil {
		return nil
	}
	var rel string
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), cgroupV2Prefix); ok {
			rel = rest
		}
	}
	if rel == "" {
		return nil
	}
	var chain []string
	for dir := filepath.Join(cgroupDir, rel); strings.HasPrefix(dir, cgroupDir+string(filepath.Separator)); dir = filepath.Dir(dir) {
		chain = append(chain, dir)
	}
	return chain
}

func parseMemoryMax(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil && v > 0
}

func parseCPUMax(s string) (int64, bool) {
	fields := strings.Fields(s)
	if len(fields) != cpuMaxFields {
		return 0, false
	}
	quota, qerr := strconv.ParseInt(fields[0], 10, 64)
	period, perr := strconv.ParseInt(fields[1], 10, 64)
	if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
		return 0, false
	}
	return (quota + period - 1) / period, true
}
