package mutation

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var policyPackages = []string{"internal/gate/policy", "internal/validate"}

func goOutput(t *testing.T, args ...string) string {
	t.Helper()
	res, err := procexec.NewOS(golang.GoBinary).Run(context.Background(), procexec.Command{Name: golang.GoBinary, Args: args})
	testutil.NoErr(t, err)
	return strings.TrimSpace(res.Stdout)
}

func engineSources(t *testing.T) string {
	t.Helper()
	root := goOutput(t, "list", "-m", "-f", "{{.Dir}}")
	listing := goOutput(t, "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{range .GoFiles}} {{.}}{{end}}{{end}}", ".")
	lines := strings.Split(listing, "\n")
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, packageSources(t, root, strings.Fields(line))...)
	}
	return hashOf(parts...)
}

func packageSources(t *testing.T, root string, fields []string) []string {
	t.Helper()
	rel, err := filepath.Rel(root, fields[0])
	if err != nil || strings.HasPrefix(rel, parentDirElem) || slices.Contains(policyPackages, filepath.ToSlash(rel)) {
		return nil
	}
	dir, err := os.OpenRoot(fields[0])
	testutil.NoErr(t, err)
	defer func() { _ = dir.Close() }()
	parts := make([]string, 0, 2*len(fields))
	for _, f := range fields[1:] {
		if rel == "internal/gate/mutation" && f == "engine.go" {
			continue
		}
		data, rerr := dir.ReadFile(f)
		testutil.NoErr(t, rerr)
		parts = append(parts, filepath.ToSlash(filepath.Join(rel, f)), string(data))
	}
	return parts
}

func TestEngineVersionCoversTheEngineSources(t *testing.T) {
	got := engineSources(t)
	if EngineVersion != got {
		t.Fatalf("the mutation engine changed: cached verdicts must not survive it; set EngineVersion in engine.go to %q", got)
	}
}
