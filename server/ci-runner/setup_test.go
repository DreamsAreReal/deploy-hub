package cirunner

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func runSetup(t *testing.T, units string, args ...string) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"./ci-runner-setup"}, args...)...)
	cmd.Env = append(os.Environ(), "CI_RUNNER_UNITS="+units, "CI_RUNNER_SYSTEMCTL=echo")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-runner-setup %v: %v: %s", args, err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func unitFiles(t *testing.T) []string {
	t.Helper()
	var units []string
	for _, pattern := range []string{"*.service", "*.timer"} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, found...)
	}
	return units
}

func TestSetupInstallsEveryUnitTemplatesIncludedAndReloads(t *testing.T) {
	dir := t.TempDir()
	calls := runSetup(t, dir, "units")
	units := unitFiles(t)
	for _, want := range []string{"ci-mutation@.service", "ci-mutation@.timer", "ci-runner@.service", "ci-runner-status.timer"} {
		if !slices.Contains(units, want) {
			t.Fatalf("%s is not a unit of the directory: %v", want, units)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range units {
		installedAsShipped(t, root, name)
	}
	if _, err := root.Stat("var-lib-ci.mount"); err == nil {
		t.Fatal("var-lib-ci.mount is installed by disk, which knows whether the image has project quotas, not by units")
	}
	if !slices.Equal(calls, []string{"daemon-reload"}) {
		t.Fatalf("systemctl calls %v, want one daemon-reload after the units", calls)
	}
}

func installedAsShipped(t *testing.T, root *os.Root, name string) {
	t.Helper()
	disk, _ := os.ReadFile(filepath.Clean(name))
	got, rerr := root.ReadFile(name)
	info, serr := root.Stat(name)
	if rerr != nil || serr != nil || !bytes.Equal(got, disk) || info.Mode().Perm() != 0o644 {
		t.Fatalf("%s is not installed as shipped: %v %v", name, rerr, serr)
	}
}

func TestSetupEnablesTheTimerOfEveryTemplateForARepo(t *testing.T) {
	calls := runSetup(t, t.TempDir(), "timers", "tasks")
	if !slices.Equal(calls, []string{"enable --now ci-mutation@tasks.timer"}) {
		t.Fatalf("systemctl calls %v", calls)
	}
}

func unitValue(t *testing.T, unit, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(unit))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return value
		}
	}
	t.Fatalf("%s has no %s", unit, key)
	return ""
}

func TestMutationChunkBudgetOutlastsTheIndexAndFitsTheTimeout(t *testing.T) {
	const unit = "ci-mutation@.service"
	_, after, ok := strings.Cut(unitValue(t, unit, "ExecStart"), "--budget ")
	if !ok {
		t.Fatalf("%s runs a chunk without --budget", unit)
	}
	budget, err := time.ParseDuration(strings.Fields(after)[0])
	if err != nil {
		t.Fatal(err)
	}
	timeout, err := time.ParseDuration(strings.Replace(unitValue(t, unit, "TimeoutStartSec"), "min", "m", 1))
	if err != nil {
		t.Fatal(err)
	}
	const indexAndBaselines = 30 * time.Minute
	if budget < 4*indexAndBaselines {
		t.Fatalf("budget %v leaves too little after %v of test index and baselines per chunk", budget, indexAndBaselines)
	}
	if timeout < budget+15*time.Minute {
		t.Fatalf("TimeoutStartSec %v kills a chunk before its budget %v ends and it keeps its verdicts", timeout, budget)
	}
}
