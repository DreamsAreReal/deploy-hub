package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestScaffoldWritesTheServiceAndPrintsTheEntries(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "notes")
	template, err := filepath.Abs("../../templates/service")
	testutil.NoErr(t, err)
	testutil.Equal(t, h.run(CmdScaffold, "notes", "--port", "9031", "--dir", dir, "--template", template), ExitOK)
	testutil.Contains(t, h.out.String(), "deploy-hub onboard DreamsAreReal/notes", "  notes:\n    repo: DreamsAreReal/notes")
	_, err = os.Stat(filepath.Join(dir, "cmd", "notes", "main.go"))
	testutil.NoErr(t, err)
	again := newHarness(t)
	testutil.Equal(t, again.run(CmdScaffold, "notes", "--port", "9031", "--dir", dir, "--template", template), ExitFail)
	testutil.Contains(t, again.err.String(), "scaffold: the target directory exists and is not empty")
	bad := newHarness(t)
	testutil.Equal(t, bad.run(CmdScaffold, "notes"), ExitUsage)
	testutil.Contains(t, bad.err.String(), "usage: deploy-hub scaffold")
}
