package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	jobAuth  = "ephemeral-job-auth-value-123"
	byocMain = "api"
)

func meta(extra ...string) string {
	lines := append([]string{"token=" + jobAuth, "actor=dev", "subject=feat: x\x07", "start=1760875000"}, extra...)
	return strings.Join(lines, "\n") + "\n"
}

func TestFirstDeploySucceeds(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, registry.LoadState(e.appDir), registry.DeployState{Current: goodTag})
	e.expectJournal("web@aaaaaaa deploy ok", "smoke=skip")
	e.expectOut(e.out, "pulling "+image+"@"+digest, "starting web @ "+goodTag, "health: ok — web@aaaaaaa live (")
	lines := e.exec.Lines()
	testutil.True(t, strings.HasPrefix(lines[0], "docker login ghcr.io -u dev"), "login first")
	testutil.Equal(t, lines[1], "docker pull -q "+image+"@"+digest)
	testutil.Equal(t, lines[2], "docker tag "+image+"@"+digest+" "+image+":"+goodTag)
	testutil.Equal(t, lines[3], "docker logout ghcr.io")
	testutil.Equal(t, e.exec.Count("docker run"), 0)
	testutil.True(t, e.locker.released, "lock released")
	e.noSecretLeak(jobAuth)
	testutil.True(t, e.metrics.has(obs.MetricLeadTime, obs.Labels{obs.LabelApp: appName}), "lead time metric")
	testutil.True(t, e.metrics.has(obs.MetricOperationDuration, obs.Labels{obs.LabelAction: "deploy", obs.LabelResult: probeOK}), "duration metric")
	e.expectLog("request accepted", "deploy identity verified", "deploy started", "lock acquired", "image pulled", "in-place recreate", "health gate finished", "journal entry", "deploy succeeded")
	testutil.True(t, e.exec.Count("up -d --pull never app") == 1 && e.exec.Count("--pull missing") == 0, "classic apps recreate only the main service from the pulled image")
	testutil.True(t, regexp.MustCompile(`web@aaaaaaa deploy ok [1-9][0-9]*s`).MatchString(e.journal()), "journal records the duration: "+e.journal())
}

func TestDeployWithoutTokenSkipsLogin(t *testing.T) {
	e := newEnv(t)
	if code := e.dispatch("deploy web "+goodTag, "start=bogus\n"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	testutil.True(t, sampleValue(e, obs.MetricLeadTime) < 60, "without a start epoch the lead time starts with the request")
	e2 := newEnv(t)
	testutil.Equal(t, e2.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.True(t, sampleValue(e2, obs.MetricLeadTime) > clock.Sub(time.Unix(1760875000, 0)).Seconds(), "lead time counts from the workflow start")
	if e.exec.Count("login") != 0 || e.exec.Count("logout") != 0 {
		t.Fatal("no token, no registry session")
	}
}

func TestBlueGreenSwapIsZeroDowntime(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag, Previous: ""})
	e.write(filepath.Join(e.appDir, ".env"), "SECRET=1\n")
	if code := e.dispatch("deploy web "+good2Tag, meta()); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	if s := registry.LoadState(e.appDir); s.Current != good2Tag || s.Previous != goodTag {
		t.Fatalf("state %+v", s)
	}
	run, ok := e.exec.Find("docker run")
	if !ok || !strings.Contains(strings.Join(run.Args, " "), "--network web_default --env-file "+filepath.Join(e.appDir, ".env")+" --memory 256m -p 127.0.0.1:19001:8080") {
		t.Fatalf("standby args: %v", run.Args)
	}
	if e.router.seen[0] != "19001" || e.router.seen[len(e.router.seen)-1] != "" {
		t.Fatalf("route must cut to standby then back: %v", e.router.seen)
	}
	if _, err := os.Stat(filepath.Join(e.appDir, registry.CaddyPortFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("route override left behind")
	}
	if e.sim.running["web-bg"] {
		t.Fatal("standby not removed")
	}
	e.expectOut(e.out, "blue-green: starting standby web-bg on 127.0.0.1:19001 -> 8080", msgCutover)
	e.expectLog("health gate finished", "standby cleaned up")
}

func TestBrokenReleaseIsDiscardedAndProdUntouched(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	if code := e.dispatch("deploy web "+badTag, meta()); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	if e.sim.images[mainName] != image+":"+goodTag || !e.sim.running[mainName] {
		t.Fatal("prod container must keep serving the old image")
	}
	if s := registry.LoadState(e.appDir); s.Current != goodTag {
		t.Fatalf("state moved: %+v", s)
	}
	if e.exec.Count("compose -f "+filepath.Join(e.appDir, "docker-compose.yml")+" up") != 0 {
		t.Fatal("main service must not be recreated")
	}
	e.expectOut(e.err, msgStandbyUnhealthy, "rolled back to previous sha (aaaaaaa) — zero-downtime: prod never changed", "no HTTP 2xx/3xx on :9001/health")
	e.expectJournal("web@dead000 deploy fail")
	if strings.Contains(e.journal(), "rollback") {
		t.Fatal("no rollback churn when prod never changed")
	}
	e.expectLog("deploy failed health gate")
}

func TestStandbyStartFailureFallsBackToRecreate(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.sim.standbyErr = errSim
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.err, msgStandbyFallback)
	if e.sim.images[mainName] != image+":"+good2Tag {
		t.Fatal("recreate did not happen")
	}
	e.expectLog("standby did not start")
	lines := e.exec.Lines()
	rm, run := slices.Index(lines, "docker rm -f web-bg"), slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "docker run -d --name web-bg") })
	testutil.True(t, rm >= 0 && run > rm, "a leftover standby is removed before the new one starts")
}

func TestStandbyFailureWithBrokenReleaseRollsBack(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	e.sim.standbyErr = errSim
	testutil.Equal(t, e.dispatch("deploy web "+badTag, ""), ExitFail)
	e.expectJournal("web@dead000 deploy fail", "web@aaaaaaa rollback ok")
	testutil.Excludes(t, e.err.String(), msgZeroDowntime)
}

func TestNonNumericPortDeploysInPlace(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.conf(registry.AppConf{Mode: registry.ModeBYOC, HMode: registry.HealthProcess, Port: "web", Image: image})
	e.sim.mainName = appName
	e.sim.running[appName] = true
	testutil.Equal(t, e.dispatch("deploy web "+good2Tag, ""), ExitOK)
	testutil.Equal(t, e.exec.Count("docker run"), 0)
}

func TestRecreateFailureAfterStandbyRestoresPrevious(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	e.sim.brokenOnMain[image+":"+good2Tag] = true
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.err, msgRestorePrevious, msgZeroDowntime)
	if e.sim.images[mainName] != image+":"+goodTag {
		t.Fatalf("previous image not restored: %s", e.sim.images[mainName])
	}
	if registry.LoadState(e.appDir).Current != goodTag {
		t.Fatal("state must not advance")
	}
	e.expectJournal("web@bbbbbbb deploy fail")
}

func TestFailedDeployRollsBackWhenOldWasDown(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag})
	if code := e.dispatch("deploy web "+badTag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@dead000 deploy fail", "web@aaaaaaa rollback ok")
	e.expectOut(e.err, "rolling back to "+goodTag, "rolled back to previous sha (aaaaaaa)")
	if e.sim.images[mainName] != image+":"+goodTag {
		t.Fatal("rollback image not running")
	}
}

func TestRollbackAfterFailureCanFailToo(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: bad2Tag})
	if code := e.dispatch("deploy web "+badTag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@dead000 rollback fail")
	e.expectOut(e.err, msgRollbackFailed)
}

func TestFirstDeployFailureStopsApp(t *testing.T) {
	e := newEnv(t)
	if code := e.dispatch("deploy web "+badTag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@dead000 stop ok 0s smoke=skip")
	e.expectOut(e.err, msgFirstFail)
	if e.sim.running[mainName] {
		t.Fatal("app must be stopped")
	}
	e2 := newEnv(t)
	e2.sim.stopErr = errSim
	e2.dispatch("deploy web "+badTag, "")
	e2.expectJournal("web@dead000 stop fail")
}

func TestPullFailureIsJournaledAndKeptOnServer(t *testing.T) {
	e := newEnv(t)
	e.sim.pullErr = errSim
	if code := e.dispatch("deploy web "+goodTag, meta()); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@aaaaaaa deploy fail")
	e.expectOut(e.err, msgPullFail+": manifest unknown")
	data, err := os.ReadFile(filepath.Join(e.hubDir, "last-error.log"))
	if err != nil || !strings.Contains(string(data), "web pull error: manifest unknown") {
		t.Fatalf("last-error.log: %q %v", data, err)
	}
	if e.exec.Count("logout") != 1 {
		t.Fatal("logout must still run")
	}
	e.noSecretLeak(jobAuth)
	e.expectLog("image pull failed")
}

func TestLastErrorUnwritableIsLogged(t *testing.T) {
	e := newEnv(t)
	e.sim.pullErr = errSim
	if err := os.Mkdir(filepath.Join(e.hubDir, "last-error.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.dispatch("deploy web "+goodTag, "")
	e.expectOut(e.logs, "last-error.log not writable")
}

func TestLoginFailureStopsDeploy(t *testing.T) {
	e := newEnv(t)
	e.sim.loginErr = errSim
	if code := e.dispatch("deploy web "+goodTag, meta()); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.err, msgLoginFail)
	e.expectJournal("web@aaaaaaa deploy fail")
	if e.exec.Count("docker pull") != 0 {
		t.Fatal("must not pull without login")
	}
	e.noSecretLeak(jobAuth)
	e.expectLog("registry login failed")
}

func TestLockBusy(t *testing.T) {
	e := newEnv(t)
	e.locker.err = errSim
	e.locker.wait = true
	if code := e.dispatch("deploy web "+goodTag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.out, msgLockWait)
	e.expectOut(e.err, "another deploy holds the lock (waited 1m0s")
	if len(e.exec.Calls()) != 0 {
		t.Fatal("nothing may run without the lock")
	}
	e.expectLog("lock not acquired")
}

func TestMissingAppConf(t *testing.T) {
	e := newEnv(t)
	if err := os.Remove(filepath.Join(e.appDir, "app.conf")); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"deploy web " + goodTag, reqRollback} {
		if code := e.dispatch(raw, ""); code != ExitFail {
			t.Fatalf("%s: exit %d", raw, code)
		}
	}
	e.expectOut(e.err, msgNoConf+" (web)")
	e.expectLog("app.conf missing")
	testutil.Equal(t, e.journal(), "")
}

func TestComposeFailuresAreLogged(t *testing.T) {
	e := newEnv(t)
	e.sim.mainName = appName
	e.conf(registry.AppConf{Mode: registry.ModeBYOC, HMode: registry.HealthProcess, Image: image})
	e.sim.upErr = errSim
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, ""), ExitFail)
	e.expectLog("sidecars up failed", "compose up failed", "deploy failed health gate")
	e.expectJournal("web@aaaaaaa deploy fail")
}

func TestRedeploySameTagKeepsRollbackTarget(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag, Previous: good2Tag})
	if code := e.dispatch("deploy web "+goodTag, ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if s := registry.LoadState(e.appDir); s.Previous != good2Tag {
		t.Fatalf("rollback target lost: %+v", s)
	}
}

func byocCompose() string {
	return base64.StdEncoding.EncodeToString([]byte("services:\n  web:\n    image: ${DEPLOY_IMAGE}\n"))
}

func TestBYOCDeployInstallsComposeAndConf(t *testing.T) {
	e := newEnv(t)
	e.sim.mainName = appName
	stdin := meta("compose="+byocCompose(), "main="+byocMain, "hport=9001", "hmode=http")
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, stdin), ExitOK)
	conf, err := registry.LoadAppConf(e.appDir)
	testutil.NoErr(t, err)
	testutil.Equal(t, conf, registry.AppConf{Mode: registry.ModeBYOC, Main: byocMain, Port: appPort, HealthPath: "/", HMode: "http", Image: image, Repo: appRepo})
	testutil.Contains(t, testutil.Read(t, filepath.Join(e.appDir, "docker-compose.yml")), "${DEPLOY_IMAGE}")
	testutil.Perm(t, filepath.Join(e.appDir, ".env"), 0o600)
	testutil.Equal(t, e.exec.Count("--no-recreate"), 1)
	testutil.True(t, e.router.syncs > 0, "BYOC deploy must refresh routes")
	validateCall, _ := e.exec.Find("config -q")
	testutil.Equal(t, validateCall.Dir, e.appDir)
	e.expectLog("byoc compose installed")
	testutil.True(t, e.exec.Count("up -d --pull missing") > 0 && e.exec.Count("--pull never") == 0, "BYOC brings the whole stack up and pulls missing sidecars")
}

func TestBYOCRejections(t *testing.T) {
	cases := []struct {
		stdin string
		msg   string
	}{
		{meta("compose=!!notbase64", "main=api", "hport=9001", "hmode=http"), msgBadCompose},
		{meta("compose="+byocCompose(), "main=bad name", "hmode=http"), msgBadMain},
		{meta("compose="+byocCompose(), "main=api", "hport=99999", "hmode=http"), msgBadHPort},
		{meta("compose="+byocCompose(), "main=api", "hmode=shell"), msgBadHMode},
	}
	for _, c := range cases {
		e := newEnv(t)
		if code := e.dispatch("deploy web "+goodTag, c.stdin); code != ExitFail {
			t.Fatalf("%s: exit %d", c.msg, code)
		}
		e.expectOut(e.err, c.msg)
		if e.exec.Count("docker pull") != 0 {
			t.Fatalf("%s: rejected deploy must not pull", c.msg)
		}
	}
}

func TestBYOCInvalidComposeKeepsOldFile(t *testing.T) {
	e := newEnv(t)
	e.write(filepath.Join(e.appDir, "docker-compose.yml"), "old\n")
	e.sim.validateErr = errSim
	stdin := meta("compose="+byocCompose(), "main=api", "hmode=process")
	if code := e.dispatch("deploy web "+goodTag, stdin); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.err, msgComposeInvalid)
	data, _ := os.ReadFile(filepath.Join(e.appDir, "docker-compose.yml"))
	if string(data) != "old\n" {
		t.Fatal("old compose must survive")
	}
	if _, err := os.Stat(filepath.Join(e.appDir, "docker-compose.yml.new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged compose must be removed")
	}
	e.expectLog("rendered compose rejected")
}

func TestBYOCWriteErrors(t *testing.T) {
	e := newEnv(t)
	if err := os.Mkdir(filepath.Join(e.appDir, "docker-compose.yml.new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := e.dispatch("deploy web "+goodTag, meta("compose="+byocCompose(), "main=api", "hmode=process")); code != ExitFail {
		t.Fatal("staging into a directory must fail")
	}
	e.expectOut(e.err, "is a directory")
	e.untouched()
	e2 := newEnv(t)
	if err := os.Mkdir(filepath.Join(e2.appDir, ".env"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e2.appDir, ".env", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e2.dispatch("deploy web "+goodTag, meta("compose="+byocCompose(), "main=api", "hmode=process")); code != ExitFail {
		t.Fatal(".env as a directory must fail")
	}
	e2.expectOut(e2.err, ".env: is a directory")
	e2.untouched()
	testutil.Missing(t, filepath.Join(e2.appDir, "docker-compose.yml.new"))
	e3 := newEnv(t)
	if err := os.Remove(filepath.Join(e3.appDir, "docker-compose.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e3.appDir, "docker-compose.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e3.appDir, "docker-compose.yml", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e3.dispatch("deploy web "+goodTag, meta("compose="+byocCompose(), "main=api", "hmode=process")); code != ExitFail {
		t.Fatal("rename onto a non-empty dir must fail")
	}
	e3.expectOut(e3.err, "docker-compose.yml")
	conf, err := registry.LoadAppConf(e3.appDir)
	testutil.NoErr(t, err)
	testutil.True(t, !conf.IsBYOC(), "app.conf must not switch to BYOC when the compose was not installed")
	testutil.Equal(t, e3.exec.Count("docker pull"), 0)
}

func (e *env) untouched() {
	e.t.Helper()
	testutil.Equal(e.t, testutil.Read(e.t, filepath.Join(e.appDir, "docker-compose.yml")), "services: {}\n")
	conf, err := registry.LoadAppConf(e.appDir)
	testutil.NoErr(e.t, err)
	testutil.True(e.t, !conf.IsBYOC(), "app.conf must stay as it was")
	testutil.Equal(e.t, e.exec.Count("docker pull"), 0)
}

func TestComposeHealthModeAndProcessMode(t *testing.T) {
	e := newEnv(t)
	e.sim.ownHealth = true
	e.conf(registry.AppConf{Profile: registry.ProfileBot, HealthPath: "/", Image: image})
	if code := e.dispatch("deploy web "+goodTag, ""); code != ExitOK {
		t.Fatalf("bot deploy exit %d", code)
	}
	e2 := newEnv(t)
	e2.sim.ownHealth = true
	e2.conf(registry.AppConf{Profile: registry.ProfileBot, Image: image})
	e2.dispatch("deploy web "+badTag, "")
	e2.expectOut(e2.err, reasonCompose)
	e3 := newEnv(t)
	e3.sim.mainName = appName
	e3.conf(registry.AppConf{Mode: registry.ModeBYOC, HMode: registry.HealthProcess, Image: image})
	if code := e3.dispatch("deploy web "+goodTag, ""); code != ExitOK {
		t.Fatalf("process deploy exit %d", code)
	}
	e4 := newEnv(t)
	e4.sim.mainName = appName
	e4.sim.crashOnUp = true
	e4.conf(registry.AppConf{Mode: registry.ModeBYOC, HMode: registry.HealthProcess, Image: image})
	if code := e4.dispatch("deploy web "+goodTag, ""); code != ExitFail {
		t.Fatalf("crashing process deploy exit %d", code)
	}
	e4.expectOut(e4.err, reasonProcess, msgFirstFail)
}

func TestSmokeSuffixRecordsConfiguredPorts(t *testing.T) {
	e := newEnv(t)
	e.write(filepath.Join(e.hubDir, "smoke.conf"), "22\n")
	e.svc.Listening = func(p int) bool { return p == 22 }
	e.dispatch("deploy web "+goodTag, "")
	e.expectJournal("deploy ok", "smoke=ok")
}

type captureNotifier struct {
	steps []delivery.Step
}

func (c *captureNotifier) Notify(_ context.Context, steps []delivery.Step) {
	c.steps = append(c.steps, steps...)
}

func TestDeployReportsDeliverySteps(t *testing.T) {
	e := newEnv(t)
	notes := &captureNotifier{}
	e.svc.Delivery = notes
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	shipped := delivery.Step{Repo: "github.com/" + appRepo, Target: delivery.DefaultTarget, SHA: registry.Sha7(goodTag), State: delivery.StateOK}
	deployed, live := shipped, shipped
	deployed.Step, live.Step = delivery.StepDeploy, delivery.StepLive
	testutil.Equal(t, notes.steps, []delivery.Step{deployed, live})
}
