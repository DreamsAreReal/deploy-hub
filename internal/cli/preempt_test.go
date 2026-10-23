package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestPreemptTakesNoArguments(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdPreempt, "now"), ExitUsage)
	testutil.Contains(t, h.err.String(), usagePreempt)
}

func TestPreemptNeedsTheHubToken(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdPreempt), ExitFail)
	testutil.Contains(t, h.err.String(), "preempt: no GitHub token")
	testutil.Missing(t, filepath.Join(h.hubDir, "preempt.json"))
}

func TestPreemptWithoutRunnersOnlyWritesItsState(t *testing.T) {
	h := newHarness(t)
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "github.token"), []byte("github_pat_x\n"), 0o600))
	testutil.Equal(t, h.run(CmdPreempt), ExitOK)
	testutil.Equal(t, testutil.Read(t, filepath.Join(h.hubDir, "preempt.json")), `{"runs":null}`)
	h.env[envRepoOwner] = "x"
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "preempt.json"), []byte("{"), 0o600))
	testutil.Equal(t, h.run(CmdPreempt), ExitFail)
}
