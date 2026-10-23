package mutation

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	engRun    = "internal/gate/mutation/run.go"
	engTest   = "internal/gate/mutation/x_test.go"
	engGate   = "internal/gate/gate.go"
	engPolicy = "internal/gate/policy/policy.go"
	engOther  = "internal/other/other.go"
)

func engineModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/hub\n\ngo 1.27\n",
		engRun:    "package mutation\n\nimport (\n\t\"example.com/hub/internal/gate\"\n\t\"example.com/hub/internal/gate/policy\"\n)\n\nvar _ = gate.X + policy.Y\n",
		engTest:   "package mutation\n",
		engGate:   "package gate\n\nconst X = 1\n",
		engPolicy: "package policy\n\nconst Y = 1\n",
		engOther:  "package other\n",
	}
	for rel, body := range files {
		testutil.Write(t, filepath.Join(dir, rel), body)
	}
	return dir
}

func TestSourceVersionFollowsTheEngineSources(t *testing.T) {
	dir := engineModule(t)
	ws := gate.Workspace{Src: dir, Exec: procexec.NewOS(golang.GoBinary), Log: slog.New(slog.DiscardHandler)}
	version := func() string {
		v, err := SourceVersion(context.Background(), ws)
		testutil.NoErr(t, err)
		return v
	}
	first := version()
	testutil.Equal(t, len(first), 64)
	steps := []struct {
		file    string
		changed bool
	}{
		{engTest, false},
		{engPolicy, false},
		{engOther, false},
		{engGate, true},
		{engRun, true},
	}
	for _, s := range steps {
		before := version()
		body := testutil.Read(t, filepath.Join(dir, s.file))
		testutil.Write(t, filepath.Join(dir, s.file), body+"\nconst changed = 2\n")
		testutil.Equal(t, version() != before, s.changed)
	}
	ws.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("exit 1") }}
	_, err := SourceVersion(context.Background(), ws)
	testutil.Fails(t, err)
}
