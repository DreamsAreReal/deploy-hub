package cirunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
