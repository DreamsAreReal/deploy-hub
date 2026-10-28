package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	envFormat = "{{json .Config.Env}}"
	portVar   = "PORT"
)

func driftEnv(t *testing.T, runEnv string, catalog *arch.Catalog) *env {
	t.Helper()
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.sim.runEnv, e.sim.bakedEnv = runEnv, `["PATH=/bin"]`
	e.svc.Catalog = catalog
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	return e
}

func TestStatusCountsWhatTheRunningAppDoesNotDeclare(t *testing.T) {
	declared := &arch.Catalog{Services: map[string]arch.Service{appName: {Secrets: []string{"SENTRY_DSN"}, Config: []string{portVar}, Exposes: []arch.Expose{{Port: 8000}}}}}
	e := driftEnv(t, `["PATH=/bin","PORT=8080","SENTRY_DSN=x","LEFTOVER_TOKEN=1"]`, declared)
	testutil.Equal(t, metricFor(e, obs.MetricArchLiveDrift, appName), 2.0)
	testutil.Contains(t, e.logs.String(), `"msg":"live drift"`, "environment variable LEFTOVER_TOKEN is set in the running container but not in secrets or config",
		"port 8080 is published by the running container but not in exposes")
	clean := driftEnv(t, `["PATH=/bin","PORT=8080"]`, &arch.Catalog{Services: map[string]arch.Service{appName: {Config: []string{portVar}, Exposes: []arch.Expose{{Port: 8080}}}}})
	testutil.Equal(t, metricFor(clean, obs.MetricArchLiveDrift, appName), 0.0)
	unknown := driftEnv(t, `["PATH=/bin"]`, &arch.Catalog{Services: map[string]arch.Service{}})
	testutil.Equal(t, metricFor(unknown, obs.MetricArchLiveDrift, appName), 2.0)
	testutil.Contains(t, unknown.logs.String(), msgNotInCatalog)
}

func TestStatusCountsNeighboursWithoutACallAndAReadableEnvFile(t *testing.T) {
	published := []arch.Expose{{Port: 8080}}
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.sim.runEnv, e.sim.bakedEnv = `["PATH=/bin"]`, `["PATH=/bin"]`
	e.sim.networks = appName + "\tshared," + appName + "_default\nother\tshared\npreview\tshared\n"
	otherDir := filepath.Join(e.hubDir, "..", "other")
	testutil.Mkdir(t, otherDir)
	e.write(filepath.Join(e.hubDir, "apps.list"), appName+" "+e.appDir+"\nother "+otherDir+"\n")
	testutil.NoErr(t, os.WriteFile(filepath.Join(e.appDir, registry.EnvFile), []byte("TOKEN=x\n"), 0o640))
	e.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: published}, "other": {}}}
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(e, obs.MetricArchLiveDrift, appName), 2.0)
	testutil.Contains(t, e.logs.String(), "docker network shared also connects other, and the catalog has no call between them",
		"secrets file .env can be read by group or others (mode 0640)")
	linked := newEnv(t)
	linked.sim.startOld(goodTag)
	linked.sim.runEnv, linked.sim.bakedEnv = `["PATH=/bin"]`, `["PATH=/bin"]`
	linked.sim.networks = e.sim.networks
	linked.write(filepath.Join(linked.hubDir, "apps.list"), appName+" "+linked.appDir+"\nother "+otherDir+"\n")
	linked.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: published, Calls: []arch.Call{{To: "other"}}}, "other": {}}}
	testutil.Equal(t, linked.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(linked, obs.MetricArchLiveDrift, appName), 0.0)
	blind := newEnv(t)
	blind.sim.startOld(goodTag)
	blind.sim.runEnv, blind.sim.bakedEnv = `["PATH=/bin"]`, `["PATH=/bin"]`
	blind.sim.networksErr = errors.New("docker down")
	blind.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: published}}}
	testutil.Equal(t, blind.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(blind, obs.MetricArchLiveDrift, appName), 0.0)
	blind.expectLog("docker networks unreadable")
}

func TestStatusSkipsDriftWithoutFactsOrCatalog(t *testing.T) {
	broken := driftEnv(t, "not json", &arch.Catalog{Services: map[string]arch.Service{}})
	testutil.Equal(t, metricFor(broken, obs.MetricArchLiveDrift, appName), -1.0)
	broken.expectLog("live facts unreadable")
	off := driftEnv(t, `["PATH=/bin","LEFTOVER=1"]`, nil)
	testutil.Equal(t, metricFor(off, obs.MetricArchLiveDrift, appName), -1.0)
	stopped := newEnv(t)
	stopped.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{}}
	testutil.Equal(t, stopped.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(stopped, obs.MetricArchLiveDrift, appName), -1.0)
}
