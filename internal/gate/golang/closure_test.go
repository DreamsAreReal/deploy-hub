package golang

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	xTestFile  = "x_test.go"
	versionOne = "v1"
)

func closureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":            "module m\n\ngo 1.27\n",
		"p/p.go":            "package p\n\nfunc F() int { return 1 }\n",
		"q/q.go":            "package q\n\nimport \"m/p\"\n\nfunc G() int { return p.F() + 1 }\n",
		"q/q_test.go":       "package q\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) { _ = G() }\n",
		"q/" + xTestFile:    "package q_test\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
		"q/testdata/in.txt": "in\n",
	}
	for rel, body := range files {
		testutil.Write(t, filepath.Join(dir, rel), body)
	}
	return dir
}

func TestListClosureOfARealModule(t *testing.T) {
	dir := closureModule(t)
	ws := gate.Workspace{Src: dir, Exec: procexec.NewOS(GoBinary), Log: slog.New(slog.DiscardHandler)}
	c, err := ListClosure(context.Background(), ws, []string{"GOFLAGS=-mod=mod"})
	testutil.NoErr(t, err)
	deps := c.Tests["m/q"]
	testutil.True(t, slices.Contains(deps, "m/p") && slices.Contains(deps, "m/q") && slices.Contains(deps, "testing"), "the test binary closure lists the package, its imports and the std library")
	_, hasP := c.Tests["m/p"]
	testutil.True(t, !hasP, "a package without tests has no test binary")
	q := c.Packages["m/q"]
	testutil.Equal(t, q.Files(), []string{"q.go", "q_test.go", xTestFile})
	testutil.True(t, c.Packages["testing"].Standard, "std packages are marked")
	_, variant := c.Packages["m/q [m/q.test]"]
	testutil.True(t, !variant, "test variants are folded into their package")
	for _, dep := range deps {
		_, listed := c.Packages[dep]
		testutil.True(t, listed, "every package of the test binary is listed: "+dep)
	}
	testutil.Equal(t, c.Packages["m/q_test"].Files(), []string{xTestFile})
	ws.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("exit 1") }}
	_, err = ListClosure(context.Background(), ws, nil)
	testutil.True(t, err != nil, "a failing go list is an error")
	_, err = DecodeClosure([]byte("{"))
	testutil.True(t, err != nil, "a broken listing is an error")
}

func TestBuiltVersioned(t *testing.T) {
	cases := []struct {
		b    Built
		want string
	}{
		{Built{}, ""},
		{Built{Module: &BuiltModule{Path: "m"}}, ""},
		{Built{Module: &BuiltModule{Path: "x", Version: versionOne}}, "x@" + versionOne},
		{Built{Module: &BuiltModule{Path: "x", Version: versionOne, Replace: &BuiltModule{Path: "../x"}}}, ""},
		{Built{Module: &BuiltModule{Path: "x", Version: versionOne, Replace: &BuiltModule{Path: "y", Version: "v2"}}}, "y@v2"},
	}
	for _, c := range cases {
		got, ok := c.b.Versioned()
		testutil.Equal(t, got, c.want)
		testutil.Equal(t, ok, c.want != "")
	}
}
