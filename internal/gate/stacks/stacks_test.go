package stacks

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const module = "example.com/app"

func workspace(t *testing.T, listErr error) gate.Workspace {
	t.Helper()
	return gate.Workspace{Src: t.TempDir(), Out: t.TempDir(), Log: slog.New(slog.DiscardHandler), Exec: &procexectest.Fake{
		Handler: func(procexec.Command) (procexec.Result, error) {
			return procexec.Result{Stdout: module + "\n"}, listErr
		},
	}}
}

func TestEveryPackImplementsEveryCheck(t *testing.T) {
	for _, p := range All() {
		for _, c := range pack.Checks {
			testutil.True(t, p.Checks[c] != nil, p.Stack+" lacks "+c)
		}
		testutil.True(t, p.Passed != nil && len(p.Sources) > 0, p.Stack+" is incomplete")
	}
	testutil.Equal(t, All()[0].Stack, StackGo)
}

func TestGoChecksKnowTheModule(t *testing.T) {
	var seen string
	run := withModule(func(_ context.Context, ws gate.Workspace) gate.Outcome {
		seen = ws.Module
		return gate.Pass(nil)
	})
	testutil.Equal(t, run(context.Background(), workspace(t, nil)).Status, gate.StatusPass)
	testutil.Equal(t, seen, module)
	failed := run(context.Background(), workspace(t, errors.New("no go.mod")))
	testutil.Equal(t, failed.Status, gate.StatusFail)
	testutil.Contains(t, failed.Reason, "go list -m")
}

func TestGoPassedTests(t *testing.T) {
	ws := workspace(t, nil)
	testutil.Write(t, ws.Report(golang.TestsReport), `{"Action":"pass","Package":"example.com/app/lib","Test":"TestA"}`+"\n")
	passed, err := goPack().Passed(context.Background(), ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"lib.TestA": true})
	_, err = goPack().Passed(context.Background(), workspace(t, errors.New("no go.mod")))
	testutil.Fails(t, err)
}
