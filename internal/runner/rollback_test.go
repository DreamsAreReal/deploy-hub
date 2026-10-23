package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestRollbackToPrevious(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(good2Tag)
	e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
	if code := e.dispatch("rollback web", ""); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	if s := registry.LoadState(e.appDir); s.Current != goodTag || s.Previous != good2Tag {
		t.Fatalf("state %+v", s)
	}
	e.expectJournal("web@aaaaaaa rollback ok")
	e.expectOut(e.out, msgWaitingGate, "rolled back to previous sha (aaaaaaa)")
	if e.exec.Count("docker pull") != 0 {
		t.Fatal("rollback must not pull")
	}
	e.expectLog("rollback started")
}

func TestRedeployExplicitTag(t *testing.T) {
	e := newEnv(t)
	e.deployedBefore(good2Tag)
	e.state(registry.DeployState{Current: goodTag})
	if code := e.dispatch("rollback web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@bbbbbbb redeploy ok")
	e.expectOut(e.out, "redeployed to requested sha (bbbbbbb)")
	if s := registry.LoadState(e.appDir); s.Current != good2Tag || s.Previous != goodTag {
		t.Fatalf("state %+v", s)
	}
}

func TestRedeployCurrentTagKeepsState(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag, Previous: good2Tag})
	if code := e.dispatch("rollback web "+goodTag, ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if s := registry.LoadState(e.appDir); s.Previous != good2Tag {
		t.Fatalf("state %+v", s)
	}
}

func TestRollbackWithoutPrevious(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag})
	if code := e.dispatch("rollback web", ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectOut(e.err, msgNoPrevious+" web")
	e.expectLog("rollback without target")
}

func TestRollbackTargetUnhealthy(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag, Previous: badTag})
	if code := e.dispatch("rollback web", ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	e.expectJournal("web@dead000 rollback fail")
	e.expectOut(e.err, "error: rollback target is not healthy")
	if registry.LoadState(e.appDir).Current != goodTag {
		t.Fatal("state must not move")
	}
	e.expectLog("rollback target unhealthy")
}

func TestRollbackNeedsAppConf(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
	testutil.NoErr(t, os.Remove(filepath.Join(e.appDir, registry.AppConfFile)))
	testutil.Equal(t, e.dispatch("rollback web", ""), ExitFail)
	e.expectOut(e.err, msgNoConf)
	testutil.Equal(t, e.journal(), "")
	testutil.Equal(t, e.exec.Count(""), 0)
}

func TestRollbackLockBusy(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag, Previous: good2Tag})
	e.locker.err = errSim
	if code := e.dispatch("rollback web", ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
}

func TestBYOCRollbackBringsSidecars(t *testing.T) {
	e := newEnv(t)
	e.sim.mainName = appName
	e.conf(registry.AppConf{Mode: registry.ModeBYOC, HMode: registry.HealthProcess, Image: image})
	e.state(registry.DeployState{Current: good2Tag, Previous: goodTag})
	if code := e.dispatch("rollback web", ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if e.exec.Count("--no-recreate") != 1 {
		t.Fatal("sidecars must be ensured")
	}
	testutil.Equal(t, e.exec.Count("--no-deps"), 0)
}

func TestBYOCDeployRecreatesChangedSidecarsButNotTheMainService(t *testing.T) {
	e := newEnv(t)
	e.sim.mainName = appName
	stack := "services:\n  web:\n    image: ${DEPLOY_IMAGE}\n  vm:\n    image: vm:1\n  vector:\n    image: vector:1\n"
	stdin := meta("compose="+base64.StdEncoding.EncodeToString([]byte(stack)), "main=web", "hport=", "hmode=process")
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, stdin), ExitOK)
	testutil.Equal(t, e.exec.Count("up -d --pull missing --no-deps vm vector"), 1)
	testutil.Equal(t, e.exec.Count("--no-deps web"), 0)
	e.sim.upErr = errSim
	e.dispatch("deploy web "+good2Tag, stdin)
	e.expectLog("changed sidecars not recreated")
}

func TestCancelledContextStillCleansUpStandby(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := e.svc.Dispatch(ctx, "rollback web "+badTag, strings.NewReader("")); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	if e.sim.running["web-bg"] {
		t.Fatal("standby leaked after cancellation")
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.appDir), "bot")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(e.hubDir, "apps.list"), "# apps\nweb "+e.appDir+"\nbot "+other+"\n")
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	e.dispatch("deploy web "+goodTag, "")
	e.out.Reset()
	if code := e.dispatch("status", ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
	if lines[0] != statusHeader || !strings.HasPrefix(lines[1], "web | aaaaaaa | running/ok | 2026-10-19T") || lines[2] != "bot | none | missing/- | never" {
		t.Fatalf("status:\n%s", e.out)
	}
	if !e.metrics.has(obs.MetricAppUp, obs.Labels{obs.LabelApp: appName}) || !e.metrics.has(obs.MetricAppsRegistered, obs.Labels{}) {
		t.Fatal("status metrics missing")
	}
	testutil.Equal(t, metricFor(e, obs.MetricAppHealthy, "web"), float64(1))
	testutil.Equal(t, metricFor(e, obs.MetricAppHealthy, "bot"), float64(0))
	testutil.Equal(t, metricFor(e, obs.MetricAppUp, "bot"), float64(0))
}

func metricFor(e *env, name, app string) float64 {
	for _, s := range e.metrics.samples {
		if s.Name == name && s.Labels[obs.LabelApp] == app {
			return s.Value
		}
	}
	return -1
}

func TestHistoryWithUnreadableJournal(t *testing.T) {
	e := newEnv(t)
	testutil.Mkdir(t, filepath.Join(e.hubDir, "deploys.log"))
	testutil.Equal(t, e.dispatch("history web", ""), ExitOK)
	e.expectOut(e.out, "no journal entries for web")
	e.expectLog("journal read failed")
}

func TestStatusProbeFailAndOwnHealth(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(badTag)
	e.dispatch("status", "")
	e.expectOut(e.out, "web | none | running/fail | never")
	e2 := newEnv(t)
	e2.sim.ownHealth = true
	e2.sim.startOld(goodTag)
	e2.dispatch("status", "")
	e2.expectOut(e2.out, "running/healthy")
}

func TestStatusWithoutAppsList(t *testing.T) {
	e := newEnv(t)
	if err := os.Remove(filepath.Join(e.hubDir, "apps.list")); err != nil {
		t.Fatal(err)
	}
	if code := e.dispatch("status", ""); code != ExitFail {
		t.Fatal("status must fail without apps.list")
	}
	e.expectOut(e.err, msgRegistryError)
	e.expectLog("apps.list unreadable")
}

func TestHistory(t *testing.T) {
	e := newEnv(t)
	e.dispatch("history web", "")
	e.expectOut(e.out, "no journal entries for web")
	for range historyLines + 5 {
		e.dispatch("deploy web "+goodTag, "")
	}
	e.out.Reset()
	if code := e.dispatch("history web", ""); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
	if len(lines) != historyLines || !strings.Contains(lines[0], "web@aaaaaaa deploy ok") {
		t.Fatalf("history: %d lines\n%s", len(lines), e.out)
	}
}

func TestJournalUnwritableIsLogged(t *testing.T) {
	e := newEnv(t)
	if err := os.Mkdir(filepath.Join(e.hubDir, "deploys.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.dispatch("deploy web "+goodTag, "")
	e.expectOut(e.logs, "journal append failed", "journal read failed")
	e.dispatch("history web", "")
}

func TestStateSaveFailuresAreLogged(t *testing.T) {
	e := newEnv(t)
	if err := os.Mkdir(filepath.Join(e.appDir, ".deploy-state.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.deployedBefore(good2Tag)
	e.dispatch("deploy web "+goodTag, "")
	e.dispatch("rollback web "+good2Tag, "")
	if strings.Count(e.logs.String(), "deploy state not saved") != 2 {
		t.Fatalf("logs: %s", e.logs)
	}
}

func TestReadMeta(t *testing.T) {
	big := strings.Repeat("A", 1<<20)
	m, err := ReadMeta(strings.NewReader("token=t\nunknown=x\nnoequals\nsubject=" + strings.Repeat("s", 200) + "\nstart=12a\ncompose=" + big + "\n"))
	if err != nil || m.Token != "t" || len(m.Subject) != subjectLimit || m.Start != "" || len(m.Compose) != len(big) || !m.BYOC() {
		t.Fatalf("meta: token=%q subject=%d start=%q compose=%d err=%v", m.Token, len(m.Subject), m.Start, len(m.Compose), err)
	}
	if err := (Meta{}).Validate(); err != nil {
		t.Fatal("template meta needs no validation")
	}
	if _, err := ReadMeta(strings.NewReader("compose=" + strings.Repeat("A", maxMetaLine+1))); err == nil {
		t.Fatal("oversized line must fail")
	}
}

func TestOversizedMetaIsRejected(t *testing.T) {
	e := newEnv(t)
	if code := e.dispatch("deploy web "+goodTag, "compose="+strings.Repeat("A", maxMetaLine+1)); code != ExitFail {
		t.Fatal("oversized stdin must be rejected")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{0: "0s", 42 * time.Second: "42s", 60 * time.Second: "1m00s", 372 * time.Second: "6m12s"}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}

func TestSudoRouter(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "deploy-hub")
	plain := filepath.Join(dir, "plain")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &procexectest.Fake{}
	(SudoRouter{Exec: f, Binary: missing, Log: log}).Sync(context.Background())
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	(SudoRouter{Exec: f, Binary: plain, Log: log}).Sync(context.Background())
	if len(f.Calls()) != 0 {
		t.Fatal("missing or non-executable binary must be skipped")
	}
	r := SudoRouter{Exec: f, Binary: bin, Log: log}
	r.Sync(context.Background())
	if f.Lines()[0] != "sudo -n "+bin+" caddy-sync" {
		t.Fatalf("got %v", f.Lines())
	}
	f.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stderr: "denied"}, errSim }
	r.Sync(context.Background())
	if !strings.Contains(logs.String(), "caddy-sync failed") || !strings.Contains(logs.String(), "caddy-sync done") {
		t.Fatalf("logs: %s", logs.String())
	}
}
