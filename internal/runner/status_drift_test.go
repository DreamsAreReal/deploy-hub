package runner

import (
	"context"
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
	appOther     = "other"
	bakedPathEnv = "[\"PATH=/bin\"]"
	backupTimer  = "app-backup.timer"
)

const (
	envFormat = "{{json .Config.Env}}"
	portVar   = "PORT"
)

func writeMode(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	testutil.NoErr(t, os.WriteFile(path, []byte(body), mode))
}

func driftEnv(t *testing.T, runEnv string, catalog *arch.Catalog) *env {
	t.Helper()
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.sim.runEnv, e.sim.bakedEnv = runEnv, bakedPathEnv
	e.svc.Catalog = catalog
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	return e
}

func TestStatusCountsWhatTheRunningAppDoesNotDeclare(t *testing.T) {
	declared := &arch.Catalog{Services: map[string]arch.Service{appName: {Secrets: []string{"SENTRY_DSN"}, Config: []string{portVar}, Exposes: []arch.Expose{{Port: 8000}, {Site: appName}}}}}
	e := driftEnv(t, `["PATH=/bin","PORT=8080","SENTRY_DSN=x","LEFTOVER_TOKEN=1"]`, declared)
	testutil.Equal(t, metricFor(e, obs.MetricArchLiveDrift, appName), 2.0)
	testutil.Contains(t, e.logs.String(), `"msg":"live drift"`, "environment variable LEFTOVER_TOKEN is set in the running container but not in secrets or config",
		"port 8080 is published by the running container but not in exposes")
	clean := driftEnv(t, `["PATH=/bin","PORT=8080"]`, &arch.Catalog{Services: map[string]arch.Service{appName: {Config: []string{portVar}, Exposes: []arch.Expose{{Port: 8080}, {Site: appName}}}}})
	testutil.Equal(t, metricFor(clean, obs.MetricArchLiveDrift, appName), 0.0)
	unknown := driftEnv(t, bakedPathEnv, &arch.Catalog{Services: map[string]arch.Service{}})
	testutil.Equal(t, metricFor(unknown, obs.MetricArchLiveDrift, appName), 3.0)
	testutil.Contains(t, unknown.logs.String(), msgNotInCatalog)
}

func TestStatusCountsAPublicSiteTheCatalogDoesNotDeclare(t *testing.T) {
	internal := &arch.Catalog{Services: map[string]arch.Service{appName: {Config: []string{portVar}, Exposes: []arch.Expose{{Port: 8080}}}}}
	public := driftEnv(t, `["PATH=/bin","PORT=8080"]`, internal)
	testutil.Equal(t, metricFor(public, obs.MetricArchLiveDrift, appName), 1.0)
	testutil.Contains(t, public.logs.String(), "public site "+appName+" is served by the hub (app.conf has no site=off) but is not in exposes")
	off := newEnv(t)
	off.conf(registry.AppConf{Profile: registry.ProfileService, Port: appPort, HealthPath: healthPath, Image: image, Site: registry.SiteOff})
	off.sim.startOld(goodTag)
	off.sim.runEnv, off.sim.bakedEnv = `["PATH=/bin","PORT=8080"]`, bakedPathEnv
	off.svc.Catalog = internal
	testutil.Equal(t, off.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(off, obs.MetricArchLiveDrift, appName), 0.0)
}

func TestStatusCountsNeighboursWithoutACallAndAReadableEnvFile(t *testing.T) {
	published := []arch.Expose{{Port: 8080}, {Site: appName}}
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.sim.runEnv, e.sim.bakedEnv = bakedPathEnv, bakedPathEnv
	e.sim.networks = appName + "\tshared," + appName + "_default\nother\tshared\npreview\tshared\n"
	otherDir := filepath.Join(e.hubDir, "..", appOther)
	testutil.Mkdir(t, otherDir)
	e.write(filepath.Join(e.hubDir, "apps.list"), appName+" "+e.appDir+"\nother "+otherDir+"\n")
	writeMode(t, filepath.Join(e.appDir, registry.EnvFile), "TOKEN=x\n", 0o640)
	e.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: published}, appOther: {}}}
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(e, obs.MetricArchLiveDrift, appName), 2.0)
	testutil.Contains(t, e.logs.String(), "docker network shared also connects other, and the catalog has no call between them",
		"secrets file .env can be read by group or others (mode 0640)")
	linked := newEnv(t)
	linked.sim.startOld(goodTag)
	linked.sim.runEnv, linked.sim.bakedEnv = bakedPathEnv, bakedPathEnv
	linked.sim.networks = e.sim.networks
	linked.write(filepath.Join(linked.hubDir, "apps.list"), appName+" "+linked.appDir+"\nother "+otherDir+"\n")
	linked.svc.Catalog = &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: published, Calls: []arch.Call{{To: appOther}}}, appOther: {}}}
	testutil.Equal(t, linked.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(linked, obs.MetricArchLiveDrift, appName), 0.0)
	blind := newEnv(t)
	blind.sim.startOld(goodTag)
	blind.sim.runEnv, blind.sim.bakedEnv = bakedPathEnv, bakedPathEnv
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

func TestStatusCountsTimersAndSitesOnTheHostNoServiceDeclares(t *testing.T) {
	declared := &arch.Catalog{Services: map[string]arch.Service{appName: {Timers: []arch.Timer{{Name: backupTimer}}}}}
	e := newEnv(t)
	e.svc.HostDirs, e.svc.Catalog = arch.HostDirs{Units: t.TempDir(), Sites: t.TempDir()}, declared
	for _, name := range []string{backupTimer, "rogue.timer"} {
		e.write(filepath.Join(e.svc.HostDirs.Units, name), "[Timer]\n")
	}
	e.write(filepath.Join(e.svc.HostDirs.Sites, "stt.caddy"), "stt {}\n")
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(e, obs.MetricArchHostDrift, ""), 2.0)
	e.expectLog("host drift")
	testutil.Contains(t, e.logs.String(), "timer rogue.timer is installed on the host but no service declares it in timers",
		"public site stt has a caddy file on the host but no service declares it in exposes")
	blind := newEnv(t)
	blind.svc.HostDirs, blind.svc.Catalog = arch.HostDirs{Units: filepath.Join(e.svc.HostDirs.Units, "rogue.timer"), Sites: e.svc.HostDirs.Sites}, declared
	testutil.Equal(t, blind.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(blind, obs.MetricArchHostDrift, ""), -1.0)
	blind.expectLog("host facts unreadable")
	uncatalogued, unset := newEnv(t), newEnv(t)
	uncatalogued.svc.HostDirs = e.svc.HostDirs
	unset.svc.Catalog = declared
	for _, off := range []*env{uncatalogued, unset} {
		testutil.Equal(t, off.dispatch("status", ""), ExitOK)
		testutil.Equal(t, metricFor(off, obs.MetricArchHostDrift, ""), -1.0)
	}
}

func TestStatusCountsOpenPortsRootUsersAndLostSandboxes(t *testing.T) {
	declared := &arch.Catalog{Admins: []arch.Admin{{User: "hub", Why: "docker"}}, Services: map[string]arch.Service{appName: {
		Exposes:  []arch.Expose{{Port: 22, Public: true}},
		Isolated: []arch.Isolation{{Unit: "runner.service", Netns: "/run/netns/runner"}},
	}}}
	e := newEnv(t)
	dirs := arch.HostDirs{Units: t.TempDir(), Net: t.TempDir(), Accounts: t.TempDir()}
	e.write(filepath.Join(dirs.Net, "tcp"), "   0: 00000000:0016 00000000:0000 0A\n   1: 00000000:1F90 00000000:0000 0A\n")
	e.write(filepath.Join(dirs.Accounts, "group"), "docker:x:988:hub,eve\n")
	e.write(filepath.Join(dirs.Units, "runner.service"), "[Service]\nUser=runner\n")
	e.svc.HostDirs, e.svc.Catalog = dirs, declared
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(e, obs.MetricArchHostDrift, ""), 3.0)
	testutil.Contains(t, e.logs.String(), "port 8080 listens beyond loopback on the host",
		"user eve can act as root through group docker", "unit runner.service runs in the host network namespace, and the catalog declares /run/netns/runner")
}

func TestStatusCountsSitesTheServerCallsWithoutAName(t *testing.T) {
	declared := &arch.Catalog{Services: map[string]arch.Service{appName: {Exposes: []arch.Expose{{Site: appName}}}}}
	seen := newEnv(t)
	seen.svc.HostDirs, seen.svc.Catalog = arch.HostDirs{Units: t.TempDir()}, declared
	seen.svc.Visits = func(context.Context) ([]arch.Visit, error) {
		return []arch.Visit{{Site: appName, Agent: "Go-http-client/1.1"}, {Site: appName, Agent: appName + "/1"}}, nil
	}
	testutil.Equal(t, seen.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(seen, obs.MetricArchHostDrift, ""), 1.0)
	testutil.Contains(t, seen.logs.String(), `site `+appName+` gets requests from the server itself with User-Agent \"Go-http-client/1.1\"`)
	blind := newEnv(t)
	blind.svc.HostDirs, blind.svc.Catalog = seen.svc.HostDirs, declared
	blind.svc.Visits = func(context.Context) ([]arch.Visit, error) { return nil, errors.New("log store down") }
	testutil.Equal(t, blind.dispatch("status", ""), ExitOK)
	testutil.Equal(t, metricFor(blind, obs.MetricArchHostDrift, ""), 0.0)
	blind.expectLog("access log unreadable")
}
