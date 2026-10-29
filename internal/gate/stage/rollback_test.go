package stage

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const deployedImage = "web:deployed"

func withData(ws gate.Workspace) gate.Workspace {
	ws.App.Data = []string{"/data"}
	ws.App.UI = &policy.UI{Seed: []string{"seed", "--db", "/data/app.db"}}
	ws.Previous = deployedImage
	return ws
}

func TestRollbackStartsTheDeployedReleaseOnTheMigratedData(t *testing.T) {
	ws, _ := app(t, okHandler())
	outcome := Rollback(context.Background(), withData(ws))
	testutil.Equal(t, outcome.Status, gate.StatusPass)
	all := strings.Join(procexectest.LinesOf(ws.Exec), "\n")
	volume := "--mount type=volume,src=deploy-hub-gate-web-rollback-data-0,dst=/data "
	testutil.Contains(t, all,
		volume+"web:gate seed --db /data/app.db",
		volume+"web:gate\n",
		"docker stop --time 10 deploy-hub-gate-web-rollback\ndocker rm -f deploy-hub-gate-web-rollback\n",
		volume+deployedImage+"\n",
		"docker volume rm -f deploy-hub-gate-web-rollback-data-0")
	testutil.True(t, strings.Index(all, volume+"web:gate\n") < strings.Index(all, volume+deployedImage), "the deployed release starts after this one")
}

func TestRollbackIsNotApplicableWithoutADeployedReleaseOrData(t *testing.T) {
	ws, _ := app(t, okHandler())
	stateless := withData(ws)
	stateless.App.Data = nil
	for _, w := range []gate.Workspace{ws, stateless} {
		outcome := Rollback(context.Background(), w)
		testutil.Equal(t, outcome.Status, gate.StatusNA)
	}
	testutil.Equal(t, len(procexectest.LinesOf(ws.Exec)), 0)
}

func TestRollbackFailsWhenTheDeployedReleaseCannotTakeOver(t *testing.T) {
	ws, sim := app(t, okHandler())
	sim.badImage = deployedImage
	outcome := Rollback(context.Background(), withData(ws))
	testutil.Equal(t, outcome.Status, gate.StatusFail)
	testutil.Contains(t, outcome.Reason, "a rollback would fail")
	testutil.Contains(t, strings.Join(outcome.Findings, " "), deployedImage+": docker run")
	broken, bsim := app(t, okHandler())
	bsim.runErr = errExit
	testutil.Contains(t, Rollback(context.Background(), withData(broken)).Reason, "does not start on seeded data")
}

func TestRollbackFailsOnServerErrorsOfTheDeployedRelease(t *testing.T) {
	ws, _ := app(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathAPI:
			w.WriteHeader(http.StatusInternalServerError)
		case "/gone":
			panic(http.ErrAbortHandler)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	ws.App.Load.Paths = []string{pathHealth, pathAPI, "/gone", "/missing"}
	outcome := Rollback(context.Background(), withData(ws))
	testutil.Equal(t, outcome.Status, gate.StatusFail)
	testutil.Equal(t, outcome.Findings, []string{pathAPI + " answered 500", "/gone answered 0"})
}
