package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestRemoteSyncNeedsAppAndTagAndARemoteSpec(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdRemoteSync, appWeb), ExitUsage)
	testutil.Contains(t, h.err.String(), usageRemoteSync)
	testutil.Equal(t, h.run(CmdRemoteSync, "tasks", "sha-abcdef1"), ExitFail)
	testutil.Contains(t, h.err.String(), "remote-sync: app has no remote spec in the gate policy: tasks")
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestRemoteDeliverChecksItsArgumentsBeforeTouchingTheTarget(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdDeliver, "ax23", "abcdef1"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageRemoteDeliver)
	testutil.Equal(t, h.run(CmdDeliver, "ax23", "abcdef1", "sh -c id"), ExitFail)
	testutil.Contains(t, h.out.String(), "via -\nresult \n")
	testutil.Contains(t, h.err.String(), "remote-deliver: invalid shipment")
	testutil.Equal(t, len(h.exec.Lines()), 0)
}

func TestRemoteDeliverRefusesAnOversizedBundle(t *testing.T) {
	var out, errs bytes.Buffer
	a := &App{
		IO:  IO{Stdin: strings.NewReader(strings.Repeat("x", maxShipment+1))},
		Out: console.New(&out), Err: console.New(&errs), Log: slog.New(slog.DiscardHandler), Now: time.Now,
	}
	testutil.Equal(t, runRemoteDeliver(context.Background(), a, nil, []string{"ax23", "abcdef1", "bundled"}), ExitFail)
	testutil.Contains(t, errs.String(), "remote-deliver: bundle is larger than 33554432 bytes")
	testutil.Equal(t, out.String(), "")
}
