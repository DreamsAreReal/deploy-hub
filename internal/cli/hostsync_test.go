package cli

import (
	"log/slog"
	"os"
	"os/user"
	"slices"
	"strconv"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestHostSyncNeedsAppAndTag(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdHostSync), ExitUsage)
	testutil.Equal(t, h.run(CmdHostSync, appWeb), ExitUsage)
	testutil.Contains(t, h.err.String(), usageHostSync)
}

func TestHostSyncRefusesAnAppWithoutAHostSpec(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdHostSync, "tasks", "sha-abcdef1"), ExitFail)
	testutil.Contains(t, h.err.String(), "host-sync: app has no host spec in the gate policy: tasks")
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestEmbeddedPolicyListsTheHostApps(t *testing.T) {
	h := newHarness(t)
	a := &App{Log: slog.New(slog.DiscardHandler)}
	testutil.True(t, slices.Contains(a.hostApps(), "observability"), "observability installs host files")
	testutil.True(t, !slices.Contains(a.hostApps(), "tasks"), "tasks has no host spec")
	testutil.True(t, slices.Contains(a.hostOnlyApps(), "server-ops") && slices.Contains(a.hostApps(), "server-ops"), "server-ops ships only host files")
	testutil.True(t, !slices.Contains(a.hostOnlyApps(), "observability"), "observability runs containers too")
	testutil.Equal(t, h.run(CmdHostSync, "Bad", "sha-abcdef1"), ExitFail)
}

func TestLookupAccountsResolvesNames(t *testing.T) {
	me, err := user.Current()
	testutil.NoErr(t, err)
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	testutil.NoErr(t, err)
	uid, gid, err := LookupAccounts(me.Username, g.Name)
	testutil.NoErr(t, err)
	testutil.Equal(t, uid, os.Getuid())
	testutil.Equal(t, gid, os.Getgid())
	_, _, err = LookupAccounts("no-such-user-x", g.Name)
	testutil.Fails(t, err)
	_, _, err = LookupAccounts(me.Username, "no-such-group-x")
	testutil.Fails(t, err)
}
