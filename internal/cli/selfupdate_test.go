package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestVersionPrintsTheBuildCommit(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdVersion), ExitOK)
	testutil.Equal(t, h.out.String(), Commit+"\n")
}

var badTagArgs = [][]string{{"v1"}, {"sha-abc"}, {"sha-" + "0123456789abcdef0123456789abcdef01234567", "surplus"}}

func TestSelfUpdateArguments(t *testing.T) {
	h := newHarness(t)
	for _, args := range badTagArgs {
		testutil.Equal(t, h.run(append([]string{CmdSelfUpdate}, args...)...), ExitUsage)
	}
	testutil.Contains(t, h.err.String(), usageSelfUpdate)
}

func TestSelfUpdateReplacesOnlyTheInstalledBinary(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdSelfUpdate, "sha-0123456789abcdef0123456789abcdef01234567"), ExitFail)
	testutil.Contains(t, h.err.String(), "self-update: "+ErrNotInstalled.Error())
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestSudoersSyncWritesTheRuleFromThePolicy(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdSudoers), ExitFail)
	testutil.Contains(t, h.err.String(), "sudoers-sync: ")
	testutil.Mkdir(t, filepath.Join(h.hubDir, "sudoers.d"))
	testutil.Equal(t, h.run(CmdSudoers), ExitOK)
	rule := testutil.Read(t, filepath.Join(h.hubDir, "sudoers.d", "deploy-hub"))
	testutil.Contains(t, rule, "deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/deploy-hub host-sync *\n", "self-update \"\"\n", "caddy-sync \"\"\n")
	testutil.Equal(t, h.exec.Count("visudo -cf"), 1)
}

func TestRunnerSyncRunsOnlyFromTheInstalledBinary(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdRunnerSync), ExitFail)
	testutil.Contains(t, h.err.String(), "ci-runner-sync: "+ErrNotInstalled.Error())
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestBootstrapArguments(t *testing.T) {
	h := newHarness(t)
	for _, args := range badTagArgs {
		testutil.Equal(t, h.run(append([]string{CmdBootstrap}, args...)...), ExitUsage)
	}
	testutil.Contains(t, h.err.String(), usageBootstrap)
}

func TestBootstrapNeedsTheHubToken(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdBootstrap), ExitFail)
	testutil.Contains(t, h.err.String(), "bootstrap: ")
	testutil.Contains(t, strings.Join(h.exec.Lines(), "\n"), "ssh vpn test ! -e /opt/deploy-hub/bin/deploy-hub", "ssh vpn stat -c %U:%a /etc/deploy-hub/github.token")
}

func TestBootstrapTakesOneTag(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdBootstrap, "sha-0123456789abcdef0123456789abcdef01234567"), ExitFail)
	testutil.Excludes(t, h.err.String(), usageBootstrap)
}
