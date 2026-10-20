package smoke

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smoke.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPorts(t *testing.T) {
	path := write(t, "# critical\n22 443\n\n  # 9999\nabc 8080 123456\n")
	if got := Ports(path); !reflect.DeepEqual(got, []int{22, 443, 8080}) {
		t.Fatalf("got %v", got)
	}
	if got := Ports(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing file: %v", got)
	}
}

func TestCheck(t *testing.T) {
	up := map[int]bool{22: true, 443: true}
	listening := func(p int) bool { return up[p] }
	if got := Check(filepath.Join(t.TempDir(), "none"), listening); got != journal.ResultSkip {
		t.Fatalf("no file: %s", got)
	}
	if got := Check(write(t, "22 443"), listening); got != journal.ResultOK {
		t.Fatalf("all up: %s", got)
	}
	if got := Check(write(t, "22 8080"), listening); got != journal.ResultFail {
		t.Fatalf("one down: %s", got)
	}
}
