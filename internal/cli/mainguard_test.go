package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestMainGuardTakesNoArguments(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdMainGuard, "now"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageMainGuard)
}

func TestMainGuardNeedsTheHubToken(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdMainGuard), ExitFail)
	testutil.Contains(t, h.err.String(), "main-guard: no GitHub token")
	testutil.Missing(t, filepath.Join(h.hubDir, mainGuardFile))
}

func TestMainGuardWithoutRunnersOnlyWritesItsState(t *testing.T) {
	h := newHarness(t)
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "github.token"), []byte("github_pat_x\n"), 0o600))
	testutil.Equal(t, h.run(CmdMainGuard), ExitOK)
	testutil.Equal(t, testutil.Read(t, filepath.Join(h.hubDir, mainGuardFile)), "{}")
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, mainGuardFile), []byte("{"), 0o600))
	testutil.Equal(t, h.run(CmdMainGuard), ExitFail)
}
