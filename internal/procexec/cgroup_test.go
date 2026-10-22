package procexec

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	hostTotal = "MemTotal:       24117248 kB\n"
	ciGroup   = "0::/user.slice/user-997.slice/ci-runner@app.service\n"
	oneByte   = "1\n"
	noLimit   = "max\n"
	noQuota   = "max 100000\n"
	sliceMem  = "user.slice/user-997.slice/memory.max"
	userMem   = "user.slice/memory.max"
	userCPU   = "user.slice/cpu.max"
)

func fakeCgroup(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	previous := cgroupDir
	cgroupDir = dir
	t.Cleanup(func() { cgroupDir = previous })
}

func TestTotalMemoryTakesTheTightestCgroupLimit(t *testing.T) {
	fakeProc(t, map[string]string{meminfoFile: hostTotal, cgroupFile: ciGroup})
	fakeCgroup(t, map[string]string{
		memoryMaxFile: oneByte,
		userMem:       noLimit,
		sliceMem:      "13958643712\n",
		"user.slice/user-997.slice/ci-runner@app.service/memory.max": noLimit,
	})
	testutil.Equal(t, TotalMemory(), int64(13958643712))
}

func TestTotalMemoryKeepsMemTotalBelowTheLimit(t *testing.T) {
	fakeProc(t, map[string]string{meminfoFile: "MemTotal: 1024 kB\n", cgroupFile: ciGroup})
	fakeCgroup(t, map[string]string{userMem: "2048000\n"})
	testutil.Equal(t, TotalMemory(), int64(1024*kilobyte))
}

func TestTotalMemoryUsesTheLimitWithoutMemTotal(t *testing.T) {
	fakeProc(t, map[string]string{meminfoFile: "MemFree: 1 kB\n", cgroupFile: ciGroup})
	fakeCgroup(t, map[string]string{userMem: "4096\n"})
	testutil.Equal(t, TotalMemory(), int64(4096))
}

func TestTotalMemoryIgnoresUnusableCgroups(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no cgroup file": {meminfoFile: hostTotal},
		"cgroup v1 only": {meminfoFile: hostTotal, cgroupFile: "4:memory:/user.slice\n"},
		"root cgroup":    {meminfoFile: hostTotal, cgroupFile: "0::/\n"},
		"outside the ns": {meminfoFile: hostTotal, cgroupFile: "0::/../../..\n"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeProc(t, files)
			fakeCgroup(t, map[string]string{memoryMaxFile: oneByte, userMem: oneByte})
			testutil.Equal(t, TotalMemory(), int64(24117248*kilobyte))
		})
	}
	fakeProc(t, map[string]string{meminfoFile: hostTotal, cgroupFile: ciGroup})
	fakeCgroup(t, map[string]string{userMem: noLimit, sliceMem: "0\n"})
	testutil.Equal(t, TotalMemory(), int64(24117248*kilobyte))
}

func TestCPUsFollowTheCPUQuota(t *testing.T) {
	fakeProc(t, map[string]string{cgroupFile: ciGroup})
	fakeCgroup(t, map[string]string{
		userCPU:                             noQuota,
		"user.slice/user-997.slice/cpu.max": "150000 100000\n",
		"user.slice/user-997.slice/ci-runner@app.service/cpu.max": "200000 100000\n",
	})
	testutil.Equal(t, CPUs(), min(runtime.NumCPU(), 2))
	fakeCgroup(t, map[string]string{userCPU: "100000 100000\n"})
	testutil.Equal(t, CPUs(), 1)
	fakeCgroup(t, map[string]string{userCPU: noQuota})
	testutil.Equal(t, CPUs(), runtime.NumCPU())
	fakeCgroup(t, map[string]string{userCPU: "1000000000 1\n"})
	testutil.Equal(t, CPUs(), runtime.NumCPU())
}

func TestParseCPUMax(t *testing.T) {
	for in, want := range map[string]int64{"600000 100000": 6, "100001 100000": 2, "50000 100000": 1, "1 1": 1} {
		got, ok := parseCPUMax(in)
		testutil.True(t, ok, in)
		testutil.Equal(t, got, want)
	}
	for _, in := range []string{"", "max 100000", "600000", "600000 100000 1", "x 100000", "600000 x", "0 100000", "-1 100000", "600000 0", "600000 -1"} {
		_, ok := parseCPUMax(in)
		testutil.True(t, !ok, in)
	}
}

func TestParseMemoryMax(t *testing.T) {
	got, ok := parseMemoryMax("13958643712")
	testutil.True(t, ok, "a byte count")
	testutil.Equal(t, got, int64(13958643712))
	for _, in := range []string{"max", "", "0", "-5"} {
		_, valid := parseMemoryMax(in)
		testutil.True(t, !valid, in)
	}
	got, ok = parseMemoryMax("1")
	testutil.True(t, ok, "one byte")
	testutil.Equal(t, got, int64(1))
}
