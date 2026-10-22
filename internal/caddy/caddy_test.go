package caddy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errSim = errors.New("simulated")

type pusher struct{ samples []obs.Sample }

const (
	testSlug = "203-0-113-7"
	webApp   = "web"
)

func (p *pusher) Push(_ context.Context, s []obs.Sample) { p.samples = append(p.samples, s...) }

func (p *pusher) value(name string) float64 {
	for _, s := range p.samples {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

type fixture struct {
	root    string
	apps    registry.Apps
	exec    *procexectest.Fake
	out     *bytes.Buffer
	logs    *bytes.Buffer
	metrics *pusher
	owned   *[]string
	syncer  Syncer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{root: root, apps: registry.Apps{Path: filepath.Join(root, "apps.list")}, exec: &procexectest.Fake{}, out: &bytes.Buffer{}, logs: &bytes.Buffer{}, metrics: &pusher{}}
	app := func(name, conf string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if conf != "" {
			if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	list := "web " + app("web", "port=9001\n") + "\n" +
		"bot " + app("bot", "profile=bot\n") + "\n" +
		"bad " + app("bad", "port=1\n") + "\n" +
		"noconf " + app("noconf", "") + "\n" +
		"api " + app("api", "port=9002\n") + "\n"
	if err := os.WriteFile(f.apps.Path, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	owned := []string{}
	f.owned = &owned
	f.syncer = Syncer{
		Config:    Config{Caddyfile: filepath.Join(root, "Caddyfile"), HostSlug: testSlug, Email: "admin@x.sslip.io", Owner: func(p string) error { owned = append(owned, p); return nil }},
		Apps:      f.apps,
		Exec:      f.exec,
		Listening: func(int) bool { return false },
		Out:       console.New(f.out),
		Log:       slog.New(slog.NewJSONHandler(f.logs, nil)),
		Metrics:   f.metrics,
	}
	return f
}

func TestRoutesAndRender(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, filepath.Join(f.root, webApp, ".caddy-port"), "19001\n")
	apps, _ := f.apps.All()
	routes := Routes(apps, nil)
	if len(routes) != 2 || routes[0] != (Route{webApp, "9001", "19001"}) || routes[1] != (Route{"api", "9002", "19002"}) {
		t.Fatalf("routes: %+v", routes)
	}
	block := "\t\tlb_try_duration 30s\n\t\tlb_try_interval 250ms\n\t\tlb_policy first\n\t\tfail_duration 5s\n\t\tmax_fails 1\n\t}\n}\n\n"
	want := header + "{\n\temail a@b\n\tgrace_period 10s\n}\n\n" +
		"import /etc/caddy/extra/*.caddy\n\n" +
		"web.s.sslip.io {\n\tlog\n\treverse_proxy 127.0.0.1:9001 127.0.0.1:19001 {\n" + block +
		"api.s.sslip.io {\n\tlog\n\treverse_proxy 127.0.0.1:9002 127.0.0.1:19002 {\n" + block
	if got := Render(routes, "s", "a@b"); got != want {
		t.Fatalf("render:\n%s", got)
	}
}

func TestStandbyPortNeverPointsAtAnotherAppAndFallsBackToSpare(t *testing.T) {
	root := t.TempDir()
	list := ""
	for _, a := range [][2]string{{webApp, "9001"}, {"other", "19001"}, {"high", "55536"}, {"edge", "55535"}} {
		dir := filepath.Join(root, a[0])
		testutil.NoErr(t, os.MkdirAll(dir, 0o750))
		testutil.Write(t, filepath.Join(dir, "app.conf"), "port="+a[1]+"\n")
		list += a[0] + " " + dir + "\n"
	}
	testutil.Write(t, filepath.Join(root, "apps.list"), list)
	apps, err := registry.Apps{Path: filepath.Join(root, "apps.list")}.All()
	testutil.NoErr(t, err)
	testutil.Equal(t, StandbyPort(apps, webApp, nil), "30000")
	testutil.Equal(t, StandbyPort(apps, "other", nil), "29001")
	testutil.Equal(t, StandbyPort(apps, "high", nil), "30001")
	testutil.Equal(t, StandbyPort(apps, "edge", nil), "65535")
	testutil.Equal(t, StandbyPort(apps, "missing", nil), "")
	busy := func(p int) bool { return p == 29001 || p == 30000 }
	testutil.Equal(t, StandbyPort(apps, webApp, busy), "30001")
	testutil.Equal(t, StandbyPort(apps, "other", busy), "30002")
	allBusy := func(p int) bool { return p >= SpareStandbyFirst }
	testutil.Equal(t, StandbyPort(apps, webApp, allBusy), "")
	testutil.Contains(t, Render([]Route{{App: webApp, Port: "9001"}}, "s", "a@b"),
		"reverse_proxy 127.0.0.1:9001 {\n\t\tlb_try_duration 30s\n\t\tlb_try_interval 250ms\n\t}\n}")
}

func TestSyncMovesStandbyUpstreamOffAPortSomethingElseListensOn(t *testing.T) {
	f := newFixture(t)
	f.syncer.Listening = func(p int) bool { return p == 19002 }
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	got := testutil.Read(t, f.syncer.Config.Caddyfile)
	testutil.Contains(t, got, "reverse_proxy 127.0.0.1:9001 127.0.0.1:19001 {", "reverse_proxy 127.0.0.1:9002 127.0.0.1:30000 {")
	f.syncer.Listening = func(p int) bool { return p == 19002 || p >= SpareStandbyFirst }
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	testutil.Contains(t, testutil.Read(t, f.syncer.Config.Caddyfile), "reverse_proxy 127.0.0.1:9002 {\n")
}

func TestSyncWritesValidatesAndReloads(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	candidate := f.syncer.Config.Caddyfile + candidateSuffix
	testutil.Equal(t, f.exec.Lines(), []string{"caddy validate --config " + candidate + " --adapter caddyfile", "systemctl reload caddy"})
	testutil.Equal(t, *f.owned, []string{candidate})
	testutil.Missing(t, candidate)
	testutil.Contains(t, testutil.Read(t, f.syncer.Config.Caddyfile), "web."+testSlug+".sslip.io")
	testutil.Perm(t, f.syncer.Config.Caddyfile, filePerm)
	testutil.Equal(t, f.out.String(), msgUpdated+"\n")
	testutil.Contains(t, f.logs.String(), `"msg":"caddyfile updated","routes":2`)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyReloads), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyRoutes), 2.0)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddySyncSuccess), 1.0)
}

func TestSyncReplacesAChangedCaddyfile(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.syncer.Config.Caddyfile, "stale\n")
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	testutil.Contains(t, testutil.Read(t, f.syncer.Config.Caddyfile), "web."+testSlug+".sslip.io")
	testutil.Equal(t, f.exec.Count("reload"), 1)
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	second := &procexectest.Fake{}
	f.syncer.Exec = second
	f.out.Reset()
	f.metrics.samples = nil
	testutil.NoErr(t, f.syncer.Run(context.Background()))
	testutil.Equal(t, len(second.Calls()), 0)
	testutil.Equal(t, f.out.String(), msgUpToDate+"\n")
	testutil.Contains(t, f.logs.String(), `"msg":"caddyfile unchanged","routes":2`)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyReloads), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyRoutes), 2.0)
}

func failing(binary string) procexectest.Handler {
	return func(c procexec.Command) (procexec.Result, error) {
		if c.Name == binary {
			return procexec.Result{Stderr: "boom"}, errSim
		}
		return procexec.Result{}, nil
	}
}

func assertFailedSync(t *testing.T, f *fixture) {
	t.Helper()
	testutil.Equal(t, f.metrics.value(obs.MetricCaddySyncSuccess), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyReloads), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricCaddyRoutes), -1.0)
	testutil.Equal(t, f.out.String(), "")
}

func TestSyncRejectsInvalidConfig(t *testing.T) {
	f := newFixture(t)
	f.exec.Handler = failing(caddyBinary)
	testutil.Is(t, f.syncer.Run(context.Background()), ErrInvalidConfig)
	testutil.Missing(t, f.syncer.Config.Caddyfile)
	testutil.Missing(t, f.syncer.Config.Caddyfile+candidateSuffix)
	testutil.Equal(t, f.exec.Count("reload"), 0)
	testutil.Contains(t, f.logs.String(), `"msg":"caddyfile invalid"`, `"output":"boom"`)
	assertFailedSync(t, f)
}

func TestSyncReloadFailure(t *testing.T) {
	f := newFixture(t)
	f.exec.Handler = failing(systemctlBinary)
	testutil.Is(t, f.syncer.Run(context.Background()), errSim)
	testutil.Contains(t, f.logs.String(), `"msg":"caddy reload failed"`)
	assertFailedSync(t, f)
}

func TestSyncOwnerFailure(t *testing.T) {
	f := newFixture(t)
	f.syncer.Config.Owner = func(string) error { return errSim }
	testutil.Is(t, f.syncer.Run(context.Background()), errSim)
	testutil.Missing(t, f.syncer.Config.Caddyfile+candidateSuffix)
	testutil.Equal(t, len(f.exec.Calls()), 0)
	assertFailedSync(t, f)
}

func TestSyncPathFailures(t *testing.T) {
	f := newFixture(t)
	f.syncer.Config.Caddyfile = filepath.Join(f.root, "missing", "Caddyfile")
	testutil.Fails(t, f.syncer.Run(context.Background()))
	assertFailedSync(t, f)
	f2 := newFixture(t)
	f2.syncer.Apps = registry.Apps{Path: filepath.Join(f2.root, "nope")}
	testutil.Fails(t, f2.syncer.Run(context.Background()))
	assertFailedSync(t, f2)
	f3 := newFixture(t)
	f3.syncer.Config.HostSlug = ""
	testutil.Is(t, f3.syncer.Run(context.Background()), ErrBadSlug)
	assertFailedSync(t, f3)
	f4 := newFixture(t)
	testutil.Mkdir(t, f4.syncer.Config.Caddyfile)
	testutil.Fails(t, f4.syncer.Run(context.Background()))
	testutil.Missing(t, f4.syncer.Config.Caddyfile+candidateSuffix)
}

func TestConfigValidate(t *testing.T) {
	ok := Config{HostSlug: testSlug, Email: "admin@" + testSlug + ".sslip.io"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{HostSlug: "bad slug; rm", Email: ok.Email}).Validate(); !errors.Is(err, ErrBadSlug) {
		t.Fatal("slug")
	}
	if err := (Config{HostSlug: ok.HostSlug, Email: "no-at\n}"}).Validate(); !errors.Is(err, ErrBadEmail) {
		t.Fatal("email")
	}
}
