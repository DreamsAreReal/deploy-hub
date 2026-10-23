package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/historysweep"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var sweepRoot = strings.Repeat("ab", 20)

func sweepHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "hub.conf"), "history_sweep=DreamsAreReal/tasks@"+sweepRoot+" DreamsAreReal/deploy-hub@"+sweepRoot+"\n")
	testutil.NoErr(t, os.WriteFile(filepath.Join(h.hubDir, "github.token"), []byte("github_pat_x\n"), 0o600))
	return h
}

func TestHistorySweepClonesEveryConfiguredRepo(t *testing.T) {
	h := sweepHarness(t)
	h.want(ExitOK, CmdHistorySweep)
	testutil.Equal(t, h.exec.Count("git clone --quiet --bare --filter=blob:none -- "+historysweep.DefaultGit+"/DreamsAreReal/tasks.git"), 1)
	testutil.Equal(t, h.exec.Count("git clone --quiet --bare --filter=blob:none -- "+historysweep.DefaultGit+"/DreamsAreReal/deploy-hub.git"), 1)
}

func TestHistorySweepFailureExitsNonZeroAndIsJournaled(t *testing.T) {
	h := sweepHarness(t)
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("no route") }
	h.want(ExitFail, CmdHistorySweep)
	testutil.Contains(t, h.err.String(), "history-sweep: DreamsAreReal/tasks: git clone: no route")
	testutil.Contains(t, testutil.Read(t, filepath.Join(h.hubDir, "deploys.log")), "deploy-hub@- history-sweep fail", "repo=DreamsAreReal/deploy-hub deleted=0")
}

func TestHistorySweepNeedsTargetsAndTheHubToken(t *testing.T) {
	h := newHarness(t)
	h.want(ExitUsage, CmdHistorySweep, "extra")
	testutil.Contains(t, h.err.String(), usageHistorySweep)
	h.want(ExitFail, CmdHistorySweep)
	testutil.Contains(t, h.err.String(), "history-sweep: "+historysweep.ErrNoTargets.Error())
	h = sweepHarness(t)
	tok := filepath.Join(h.hubDir, "github.token")
	testutil.NoErr(t, os.Rename(tok, tok+".real"))
	testutil.NoErr(t, os.Symlink(tok+".real", tok))
	h.want(ExitFail, CmdHistorySweep)
	testutil.Contains(t, h.err.String(), "history-sweep: token file must be")
	testutil.NoErr(t, os.Remove(filepath.Join(h.hubDir, "github.token")))
	h.want(ExitFail, CmdHistorySweep)
	testutil.Contains(t, h.err.String(), "history-sweep: no GitHub token")
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestHistorySweepTargetsComeFromHubConf(t *testing.T) {
	h := newHarness(t)
	cfg, err := LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.HistorySweep, "")
	h = sweepHarness(t)
	cfg, err = LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Contains(t, cfg.HistorySweep, "DreamsAreReal/tasks@"+sweepRoot)
}
