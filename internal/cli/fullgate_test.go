package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestFullGateTakesNoArguments(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdFullGate, "tasks"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageFullGate)
}

func TestFullGateNeedsTheHubToken(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdFullGate), ExitFail)
	testutil.Contains(t, h.err.String(), "full-gate: no GitHub token")
	testutil.Missing(t, filepath.Join(h.hubDir, "full-gate.json"))
}

func TestFullGateWithoutRunnersOnlyWritesItsState(t *testing.T) {
	h := newHarness(t)
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "github.token"), []byte("github_pat_x\n"), 0o600))
	testutil.Equal(t, h.run(CmdFullGate), ExitOK)
	testutil.Equal(t, testutil.Read(t, filepath.Join(h.hubDir, "full-gate.json")), `{"results":{},"since":{},"tried":{},"night":""}`)
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "full-gate.json"), []byte("{"), 0o600))
	testutil.Equal(t, h.run(CmdFullGate), ExitFail)
	testutil.Contains(t, h.err.String(), "full-gate: ")
}
