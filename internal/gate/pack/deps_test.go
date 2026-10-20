package pack

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	stamp   = "probe.ready"
	leftPad = "left-pad"
)

func TestApprovalNormalizesNames(t *testing.T) {
	ws := gate.Workspace{Policy: policy.Policy{Dependencies: []string{"npm:Left-Pad", "pypi:left-pad", "example.com/go"}}}
	out := Approval(ws, "npm:", []string{leftPad, "right-pad"}, strings.ToLower)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, reasonDeps)
	testutil.Equal(t, out.Findings, []string{"npm:right-pad is not in the approved dependency list"})
	testutil.Equal(t, out.Values, map[string]float64{ValueDependencies: 2})
	testutil.Equal(t, Approval(ws, "pypi:", []string{leftPad}, strings.ToLower).Status, gate.StatusPass)
}

func TestLockedPreparesOnlyCleanLocks(t *testing.T) {
	calls := 0
	prepare := func(context.Context, gate.Workspace) error {
		calls++
		return nil
	}
	dirty := Locked(context.Background(), gate.Workspace{}, "unlocked", []string{"a.lock missing"}, prepare)
	testutil.Equal(t, dirty.Status, gate.StatusFail)
	testutil.Equal(t, calls, 0)
	testutil.Equal(t, Locked(context.Background(), gate.Workspace{}, "unlocked", nil, prepare).Status, gate.StatusPass)
	testutil.Equal(t, calls, 1)
	broken := Locked(context.Background(), gate.Workspace{}, "unlocked", nil, func(context.Context, gate.Workspace) error { return errSim })
	testutil.Equal(t, broken.Findings, []string{errSim.Error()})
}

func TestPreparedRunsAfterPreparation(t *testing.T) {
	ready := Prepared(func(context.Context, gate.Workspace) error { return nil })
	pass := func(context.Context, gate.Workspace) gate.Outcome { return gate.Pass(nil) }
	testutil.Equal(t, ready(pass)(context.Background(), gate.Workspace{}).Status, gate.StatusPass)
	failing := Prepared(func(context.Context, gate.Workspace) error { return errSim })
	out := failing(pass)(context.Background(), gate.Workspace{})
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, errSim.Error())
}

func TestOnceRunsStepsUntilStamped(t *testing.T) {
	ws := gate.Workspace{Out: t.TempDir()}
	var order []string
	step := func(name string, err error) func(context.Context, gate.Workspace) error {
		return func(context.Context, gate.Workspace) error {
			order = append(order, name)
			return err
		}
	}
	testutil.Is(t, Once(context.Background(), ws, stamp, step("a", nil), step("b", errSim), step("c", nil)), errSim)
	testutil.Equal(t, order, []string{"a", "b"})
	testutil.Missing(t, ws.Report(stamp))
	testutil.NoErr(t, Once(context.Background(), ws, stamp, step("d", nil)))
	testutil.NoErr(t, Once(context.Background(), ws, stamp, step("e", errors.New("not run"))))
	testutil.Equal(t, order, []string{"a", "b", "d"})
	testutil.Equal(t, testutil.Read(t, ws.Report(stamp)), stamp)
	blocked := gate.Workspace{Out: filepath.Join(t.TempDir(), "missing")}
	testutil.Fails(t, Once(context.Background(), blocked, stamp))
}
