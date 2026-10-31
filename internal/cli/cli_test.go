package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/catalog"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hub"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	verbStatus = "status"
)

const (
	sourcePrune = "prune"
	runnerOther = "runner-other"
)

var errSim = errors.New("simulated")

const (
	appWeb    = "web"
	oneSecond = "1s"
)

type harness struct {
	t          *testing.T
	env        map[string]string
	exec       *procexectest.Fake
	out        *bytes.Buffer
	err        *bytes.Buffer
	hubDir     string
	owned      []string
	grouped    []string
	groupErr   error
	gateChecks []gate.Check
}

const (
	relativeDir    = "relative/dir"
	unreachableURL = "http://127.0.0.1:1/x"
	testSlug       = "203-0-113-7"
	testAddr       = "192.0.2.10"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, exec: &procexectest.Fake{}, out: &bytes.Buffer{}, err: &bytes.Buffer{}, hubDir: filepath.Join(root, "deploy-hub")}
	if err := os.MkdirAll(h.hubDir, 0o750); err != nil {
		t.Fatal(err)
	}
	h.env = map[string]string{envHubDir: h.hubDir, envMetricsURL: "", envLogsURL: "", envCaddyfile: filepath.Join(root, "Caddyfile"), envHome: root, envHostSlug: testSlug, envVPSAddr: testAddr}
	return h
}

func (h *harness) checks() []gate.Check {
	if h.gateChecks != nil {
		return h.gateChecks
	}
	return catalog.Checks()
}

func (h *harness) lookup(key string) (string, bool) {
	v, ok := h.env[key]
	return v, ok
}

func (h *harness) run(args ...string) int {
	deps := Deps{
		Exec: func(...string) procexec.Runner { return h.exec },
		Owner: func(string) func(string) error {
			return func(p string) error { h.owned = append(h.owned, p); return nil }
		},
		Group: func(g string) func(string) error {
			return func(p string) error { h.grouped = append(h.grouped, g+":"+p); return h.groupErr }
		},
		Now:          time.Now,
		Checks:       h.checks,
		Sudoers:      filepath.Join(h.hubDir, "sudoers.d", "deploy-hub"),
		Token:        filepath.Join(h.hubDir, "github.token"),
		CIRoot:       filepath.Join(h.hubDir, "ci"),
		PreemptState: filepath.Join(h.hubDir, "preempt.json"),
		Restarts:     filepath.Join(h.hubDir, "host-restart"),
	}
	return Run(context.Background(), args, h.lookup, IO{Stdin: strings.NewReader(""), Stdout: h.out, Stderr: h.err}, deps)
}

func (h *harness) want(code int, args ...string) {
	h.t.Helper()
	if got := h.run(args...); got != code {
		h.t.Fatalf("%v: exit %d, want %d: %s", args, got, code, h.err)
	}
}

func (h *harness) write(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func TestUsageAndUnknownCommand(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != ExitUsage {
		t.Fatalf("no args: %d", code)
	}
	if code := h.run("explode"); code != ExitUsage {
		t.Fatalf("unknown: %d", code)
	}
	if !strings.Contains(h.err.String(), "usage: deploy-hub <command>") {
		t.Fatal("usage text")
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		envHubDir:      relativeDir,
		envMetricsURL:  "ftp://x",
		envLogsURL:     "gopher://logs",
		envTextfileDir: relativeDir,
		envDeployUser:  "Bad User",
		envHostSlug:    "bad slug",
		envHubRepo:     "no-slash",
		envGateTimeout: "10s",
		envGateTick:    "soon",
	}
	for key, value := range cases {
		h := newHarness(t)
		h.env[key] = value
		if code := h.run(CmdFacts, appWeb); code != ExitFail {
			t.Fatalf("%s=%q accepted", key, value)
		}
	}
	for _, v := range []string{"http://127.0.0.1:8428/api/v1/import/prometheus", "https://vm.example/api", ""} {
		if err := validMetricsURL(v); err != nil {
			t.Fatalf("%q rejected: %v", v, err)
		}
	}
	if err := validMetricsURL("http://"); !errors.Is(err, ErrMetricsURL) {
		t.Fatal("host required")
	}
	if err := validMetricsURL("http://a b/"); !errors.Is(err, ErrMetricsURL) {
		t.Fatal("unparsable url")
	}
}

const (
	maxTimeout = "10m"
	maxTick    = "30s"
)

func TestGateTiming(t *testing.T) {
	cases := []struct {
		timeout, tick string
		ok            bool
	}{{"", "", true}, {"60s", oneSecond, true}, {maxTimeout, maxTick, true}, {"59s", oneSecond, false}, {"11m", oneSecond, false}, {"90s", "500ms", false}, {"2m", "31s", false}, {"x", "", false}}
	for _, c := range cases {
		h := newHarness(t)
		h.env[envGateTimeout], h.env[envGateTick] = c.timeout, c.tick
		_, err := LoadConfig(h.lookup)
		testutil.True(t, (err == nil) == c.ok, c.timeout+"/"+c.tick)
	}
	h := newHarness(t)
	cfg, err := LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Equal(t, []time.Duration{cfg.GateWait, cfg.GateTick}, []time.Duration{health.Timeout, health.Interval})
	h.env[envGateTimeout], h.env[envGateTick] = maxTimeout, maxTick
	cfg, err = LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Equal(t, []time.Duration{cfg.GateWait, cfg.GateTick}, []time.Duration{10 * time.Minute, 30 * time.Second})
}

func TestBlankValuesFallBackToDefaults(t *testing.T) {
	cfg, err := LoadConfig(func(string) (string, bool) { return "", true })
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Paths.Dir, hub.DefaultDir)
	testutil.Equal(t, cfg.DeployUser, defaultDeployUser)
	testutil.Equal(t, cfg.MetricsURL, "")
	testutil.Equal(t, cfg.LogsURL, "")
	emailOnly := map[string]string{envACMEEmail: "ops@example.org"}
	_, err = LoadConfig(func(k string) (string, bool) { v, ok := emailOnly[k]; return v, ok })
	testutil.Is(t, err, caddy.ErrBadSlug)
}

func TestDefaultsAndMetricsToggle(t *testing.T) {
	cfg, err := LoadConfig(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paths.Dir != "/opt/deploy-hub" || cfg.MetricsURL != defaultMetricsURL || cfg.Caddy.Email != "" || cfg.Caddy.HostSlug != "" || cfg.VPSAddr != "" || cfg.HostAlias != "vpn" || cfg.DeployKey != defaultDeployKey {
		t.Fatalf("defaults %+v", cfg)
	}
	testutil.Equal(t, cfg.WorkflowRef(), "DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@refs/heads/main")
	testutil.Equal(t, cfg.LogsURL, "http://127.0.0.1:9428/select/logsql/query")
	testutil.Equal(t, cfg.Audience, defaultAudience)
	a := &App{Config: Config{MetricsURL: unreachableURL}, Now: time.Now}
	p, ok := a.metrics().(obs.HTTPPusher)
	testutil.True(t, ok && p.URL == unreachableURL && p.Client != nil, "HTTP pusher for a configured URL")
	_, off := (&App{}).metrics().(obs.Discard)
	testutil.True(t, off, "an empty URL disables metrics")
	dir := t.TempDir()
	both := &App{Config: Config{MetricsURL: unreachableURL, TextfileDir: dir}, Now: time.Now, source: sourcePrune}
	fanout, fans := both.metrics().(obs.Fanout)
	again, _ := both.metrics().(obs.Fanout)
	testutil.True(t, fans && len(fanout) == 2 && len(again) == 2 && again[1] == fanout[1], "push and one shared textfile for the whole run")
	only := &App{Config: Config{TextfileDir: dir}, Now: time.Now, source: sourcePrune}
	file, alone := only.metrics().(*obs.Textfile)
	testutil.True(t, alone && file.Path() == filepath.Join(dir, "deploy-hub-prune.prom"), "a textfile alone")
	for request, want := range map[string]string{verbStatus: "runner-status", "  deploy web sha-1": "runner-deploy", "Bad;verb": runnerOther, "": runnerOther} {
		testutil.Equal(t, textfileSource(CmdRunner, request), want)
	}
	testutil.Equal(t, textfileSource(CmdPrune, verbStatus), CmdPrune)
}

func TestRenderCommand(t *testing.T) {
	h := newHarness(t)
	in := filepath.Join(t.TempDir(), "compose.yml")
	out := filepath.Join(t.TempDir(), "rendered.yml")
	h.write(in, "services:\n  web:\n    build: .\n    ports: ['8000:8000']\n")
	if code := h.run(CmdRender, appWeb, in, out); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.HasPrefix(h.out.String(), "main=web\nport=8000\npublic=true\nhealth=http\n") {
		t.Fatalf("meta %q", h.out)
	}
	testutil.Contains(t, testutil.Read(t, out), "${DEPLOY_IMAGE:?")
	bad := filepath.Join(t.TempDir(), "bad.yml")
	h.write(bad, "services: {}\n")
	cases := [][]string{
		{CmdRender, appWeb},
		{CmdRender, "Bad App", in, out},
		{CmdRender, appWeb, filepath.Join(t.TempDir(), "missing"), out},
		{CmdRender, appWeb, in, filepath.Join(t.TempDir(), "no", "out.yml")},
		{CmdRender, appWeb, bad, out},
	}
	for _, c := range cases {
		if code := h.run(c...); code == ExitOK {
			t.Fatalf("%v accepted", c)
		}
	}
	if !strings.Contains(h.err.String(), "compose-render: no services in compose") {
		t.Fatalf("stderr %s", h.err)
	}
}

func TestFactsCommand(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "apps.list"), "web\n")
	if code := h.run(CmdFacts, appWeb); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "dir="+filepath.Join(filepath.Dir(h.hubDir), appWeb)+"\nin_list=1\n") {
		t.Fatalf("facts %q", h.out)
	}
	for _, args := range [][]string{{CmdFacts}, {CmdFacts, "../x"}} {
		if code := h.run(args...); code != ExitUsage {
			t.Fatalf("%v: %d", args, code)
		}
	}
	testutil.Contains(t, testutil.Read(t, filepath.Join(h.hubDir, "logs", "facts.log")), `"msg":"command finished"`)
}

func TestRunnerCommandUsesSSHOriginalCommand(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "apps.list"), "web\n")
	h.env[envSSHOriginal] = verbStatus
	if code := h.run(CmdRunner, "ignored"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.HasPrefix(h.out.String(), "app | sha | health | last deploy\nweb | none | missing/- | never\n") {
		t.Fatalf("status %q", h.out)
	}
	h2 := newHarness(t)
	if code := h2.run(CmdRunner, "reboot", "now"); code != ExitFail {
		t.Fatal("argv request must be refused")
	}
	testutil.Contains(t, testutil.Read(t, filepath.Join(h2.hubDir, "deploys.log")), "refuse denied(reboot_now)")
	h3 := newHarness(t)
	h3.env[envHubDir] = filepath.Join(t.TempDir(), "absent")
	if code := h3.run(CmdRunner, verbStatus); code != ExitFail {
		t.Fatal("status without apps.list must fail")
	}
}

func TestCaddySyncPruneProvision(t *testing.T) {
	h := newHarness(t)
	appDir := filepath.Join(filepath.Dir(h.hubDir), appWeb)
	h.write(filepath.Join(h.hubDir, "apps.list"), "web\n")
	h.write(filepath.Join(appDir, "app.conf"), "port=9001\nimage=ghcr.io/o/web\n")
	h.want(ExitOK, CmdCaddySync)
	testutil.Equal(t, len(h.owned), 1)
	h.want(ExitOK, CmdPrune)
	h.want(ExitOK, CmdProvision, "--app", "api", "--profile", "service", "--port", "9002", "--cport", "80", "--mem", "64m", "--image", "ghcr.io/o/api", "--repo", "o/api")
	testutil.Contains(t, testutil.Read(t, filepath.Join(filepath.Dir(h.hubDir), "api", "app.conf")), "repo=o/api")
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	h.want(ExitOK, CmdCaddySync)
	testutil.NoErr(t, os.Remove(h.env[envCaddyfile]))
	h.want(ExitFail, CmdCaddySync)
	h.want(ExitUsage, CmdProvision, "--bogus")
	h.want(ExitFail, CmdProvision, "--app", "x", "--profile", "nope")
	h.env[envHubDir] = filepath.Join(t.TempDir(), "absent")
	h.want(ExitFail, CmdPrune)
}

func TestServerConfigFile(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "hub.conf"), "host_slug="+testSlug+"\nacme_email=ops@example.org\n")
	env := map[string]string{envHubDir: dir}
	cfg, err := LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Caddy.HostSlug, testSlug)
	testutil.Equal(t, cfg.Caddy.Email, "ops@example.org")
	env[envHostSlug] = "other-slug"
	cfg, err = LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Caddy.HostSlug, "other-slug")
	testutil.Write(t, filepath.Join(dir, "hub.conf"), "host_slug="+testSlug+"\n")
	cfg, err = LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Caddy.Email, "admin@other-slug.sslip.io")
	for key, bad := range map[string]string{envHostSlug: "Bad Slug", envVPSAddr: "evil host;id", envACMEEmail: "nope"} {
		broken := map[string]string{envHubDir: dir, key: bad}
		_, err := LoadConfig(func(k string) (string, bool) { v, ok := broken[k]; return v, ok })
		testutil.Fails(t, err)
	}
}

func TestOnboardNeedsVPSAddress(t *testing.T) {
	h := newHarness(t)
	delete(h.env, envVPSAddr)
	testutil.Equal(t, h.run(CmdOnboard, "owner/repo"), ExitUsage)
	testutil.Contains(t, h.err.String(), "set VPS_ADDR")
	testutil.Equal(t, len(h.exec.Calls()), 0)
}

func TestCaddySyncNeedsHostSlug(t *testing.T) {
	h := newHarness(t)
	delete(h.env, envHostSlug)
	h.write(filepath.Join(h.hubDir, "apps.list"), "")
	testutil.Equal(t, h.run(CmdCaddySync), ExitFail)
	testutil.Contains(t, h.err.String(), "invalid HOST_SLUG")
}

func TestOnboardCommand(t *testing.T) {
	h := newHarness(t)
	if code := h.run(CmdOnboard, "--help"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "usage: deploy-hub onboard") {
		t.Fatal("help text")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errSim }

func TestOutputFailureFailsCommand(t *testing.T) {
	h := newHarness(t)
	deps := Deps{Exec: func(...string) procexec.Runner { return h.exec }, Owner: ChownTo, Now: time.Now}
	code := Run(context.Background(), []string{CmdOnboard, "--help"}, h.lookup, IO{Stdin: strings.NewReader(""), Stdout: brokenWriter{}, Stderr: h.err}, deps)
	if code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	testutil.Contains(t, h.err.String(), `"msg":"output write failed"`, errSim.Error())
}

func TestConfigErrorIsPrinted(t *testing.T) {
	h := newHarness(t)
	h.env[envHubDir] = relativeDir
	testutil.Equal(t, h.run(CmdPrune), ExitFail)
	testutil.Equal(t, strings.TrimSpace(h.err.String()), ErrHubDir.Error())
}

func TestLoggerFallbacks(t *testing.T) {
	var stderr bytes.Buffer
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log, closeLog := openLogger(Config{LogFile: filepath.Join(blocker, "x.log")}, "runner", handler{onServer: true}, &stderr, &obs.LevelCounts{})
	log.Warn("visible")
	closeLog()
	if !strings.Contains(stderr.String(), "log file unavailable") || !strings.Contains(stderr.String(), "visible") {
		t.Fatalf("stderr %s", stderr.String())
	}
	stderr.Reset()
	log, closeLog = openLogger(Config{}, "render", handler{}, &stderr, &obs.LevelCounts{})
	log.Info("hidden")
	log.Warn("shown")
	closeLog()
	if strings.Contains(stderr.String(), "hidden") || !strings.Contains(stderr.String(), "shown") {
		t.Fatalf("local commands default to warn: %s", stderr.String())
	}
	stderr.Reset()
	log, _ = openLogger(Config{LogLevel: "debug"}, "render", handler{}, &stderr, &obs.LevelCounts{})
	log.Debug("dbg")
	if !strings.Contains(stderr.String(), "dbg") {
		t.Fatal("explicit level wins")
	}
	stderr.Reset()
	log, _ = openLogger(Config{}, CmdGate, handler{verbose: true}, &stderr, &obs.LevelCounts{})
	log.Info("progress")
	testutil.Contains(t, stderr.String(), "progress")
}

func TestChownTo(t *testing.T) {
	u, err := user.Current()
	testutil.NoErr(t, err)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ChownTo(u.Username)(path); err != nil {
		t.Fatalf("chown to self: %v", err)
	}
	if err := ChownTo("deploy-hub-no-such-user")(path); err == nil {
		t.Fatal("unknown user must fail")
	}
}

func TestOSDeps(t *testing.T) {
	d := OSDeps()
	if _, err := d.Exec("sh").Run(context.Background(), procexec.Command{Name: "rm"}); !errors.Is(err, procexec.ErrNotAllowed) {
		t.Fatal("allowlist must be enforced")
	}
	if d.Now().IsZero() || d.Owner == nil {
		t.Fatal("deps")
	}
	testutil.Equal(t, len(d.Checks()), len(catalog.Checks()))
}

func TestMainEntry(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(context.Background(), nil, func(string) (string, bool) { return "", false }, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestLogsURLTurnsTheAccessLogCheckOnAndOff(t *testing.T) {
	_, err := LoadConfig(func(k string) (string, bool) {
		if k == envLogsURL {
			return "logs store", true
		}
		return "", false
	})
	testutil.Is(t, err, ErrLogsURL)
	off := &App{Config: Config{}}
	testutil.True(t, off.visits() == nil, "no logs URL, no access log check")
	on := &App{Config: Config{LogsURL: "http://127.0.0.1:1/select/logsql/query"}}
	visits := on.visits()
	testutil.True(t, visits != nil, "a logs URL turns the check on")
	_, err = visits(context.Background())
	testutil.Fails(t, err)
}
