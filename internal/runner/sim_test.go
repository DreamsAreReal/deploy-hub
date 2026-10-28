package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hub"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	goodTag    = "sha-aaaaaaa1111"
	good2Tag   = "sha-bbbbbbb2222"
	badTag     = "sha-dead0001111"
	bad2Tag    = "sha-dead0002222"
	image      = "ghcr.io/owner/web"
	appName    = "web"
	mainName   = "web-app-1"
	appRepo    = "Owner/web"
	hubRef     = "Owner/deploy-hub/.github/workflows/deploy.yml@refs/heads/main"
	goodOIDC   = "signed-by-github"
	digest     = "sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	appPort    = "9001"
	psVerb     = "ps"
	verbStop   = "stop"
	verbInsp   = "inspect"
	healthPath = "/health"
	rwData     = "/data\n"

	formatRWMounts = "{{range .Mounts}}{{if .RW}}{{.Destination}}\n{{end}}{{end}}"
)

var (
	errSim = errors.New("simulated failure")
	clock  = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
)

type sim struct {
	mu           sync.Mutex
	running      map[string]bool
	images       map[string]string
	ports        map[string]string
	ownHealth    bool
	mainName     string
	pullErr      error
	loginErr     error
	logoutErr    error
	rmErr        error
	standbyErr   error
	validateErr  error
	stopErr      error
	brokenOnMain map[string]bool
	crashOnUp    bool
	upErr        error
	rwMounts     string
	mountsErr    error
	imageLabel   string
	runEnv       string
	bakedEnv     string
	stopped      []string
}

func newSim() *sim {
	return &sim{running: map[string]bool{}, images: map[string]string{}, ports: map[string]string{}, mainName: mainName, brokenOnMain: map[string]bool{}}
}

func envImage(c procexec.Command) string {
	for _, e := range c.Env {
		if v, ok := strings.CutPrefix(e, docker.ImageEnv+"="); ok {
			return v
		}
	}
	return ""
}

func (s *sim) healthy(container string) bool {
	img := s.images[container]
	if !s.running[container] || strings.Contains(img, ":sha-dead") {
		return false
	}
	return container != s.mainName || !s.brokenOnMain[img]
}

func (s *sim) handle(c procexec.Command) (procexec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := c.Args
	switch a[0] {
	case "login":
		return procexec.Result{}, s.loginErr
	case "logout":
		return procexec.Result{}, s.logoutErr
	case "pull":
		if s.pullErr != nil {
			return procexec.Result{Stderr: "manifest unknown"}, s.pullErr
		}
	case "compose":
		return s.compose(c)
	case psVerb:
		name := strings.TrimSuffix(strings.TrimPrefix(a[3], "name=^"), "$")
		if s.running[name] {
			return procexec.Result{Stdout: "id123\n"}, nil
		}
	case verbInsp:
		return s.inspectCall(a)
	}
	return s.lifecycle(a)
}

func (s *sim) inspectCall(a []string) (procexec.Result, error) {
	if a[2] == formatRWMounts {
		return procexec.Result{Stdout: s.rwMounts}, s.mountsErr
	}
	return s.inspect(a[2], a[3]), nil
}

func (s *sim) lifecycle(a []string) (procexec.Result, error) {
	switch a[0] {
	case "image":
		if a[1] == verbInsp && a[3] == envFormat {
			return procexec.Result{Stdout: s.bakedEnv}, nil
		}
		if a[1] == verbInsp {
			return procexec.Result{Stdout: s.imageLabel + "\n"}, nil
		}
	case verbStop:
		s.stopped = append(s.stopped, a[1])
		s.running[a[1]] = false
	case "rm":
		if s.rmErr != nil {
			return procexec.Result{}, s.rmErr
		}
		delete(s.running, a[2])
	}
	return procexec.Result{}, nil
}

func (s *sim) compose(c procexec.Command) (procexec.Result, error) {
	switch c.Args[3] {
	case "up":
		if s.upErr != nil {
			return procexec.Result{}, s.upErr
		}
		if !slices.Contains(c.Args, "--no-recreate") {
			s.running[s.mainName] = !s.crashOnUp
			s.images[s.mainName] = envImage(c)
			s.ports[appPort] = s.mainName
		}
	case verbStop:
		if s.stopErr != nil {
			return procexec.Result{}, s.stopErr
		}
		s.running[s.mainName] = false
	case "run":
		return s.run(c)
	case "config":
		return procexec.Result{}, s.validateErr
	case psVerb:
		if _, err := os.Stat(c.Args[2]); err != nil {
			return procexec.Result{}, err
		}
		return procexec.Result{Stdout: s.mainName + "\n"}, nil
	}
	return procexec.Result{}, nil
}

func (s *sim) inspect(format, container string) procexec.Result {
	switch format {
	case "{{.State.Status}}":
		if s.running[container] {
			return procexec.Result{Stdout: health.StatusRunning}
		}
		if _, known := s.images[container]; known {
			return procexec.Result{Stdout: "exited"}
		}
		return procexec.Result{}
	case "{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}":
		if !s.ownHealth {
			return procexec.Result{Stdout: "-"}
		}
		if s.healthy(container) {
			return procexec.Result{Stdout: health.StatusHealthy}
		}
		return procexec.Result{Stdout: "unhealthy"}
	case "{{range $p,$c := .NetworkSettings.Ports}}{{range $c}}{{$p}} {{.HostPort}}\n{{end}}{{end}}":
		return procexec.Result{Stdout: "8080/tcp " + appPort + "\n"}
	case envFormat:
		return procexec.Result{Stdout: s.runEnv}
	default:
		return procexec.Result{Stdout: "web_default\n"}
	}
}

func (s *sim) run(c procexec.Command) (procexec.Result, error) {
	if s.standbyErr != nil {
		return procexec.Result{}, s.standbyErr
	}
	a := c.Args
	name := a[slices.Index(a, "--name")+1]
	s.running[name] = true
	s.images[name] = envImage(c)
	if i := slices.Index(a, "-p"); i > 0 {
		s.ports[strings.Split(a[i+1], ":")[1]] = name
	}
	return procexec.Result{}, nil
}

func (s *sim) HTTPOK(_ context.Context, raw string, _ time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return s.healthy(s.ports[u.Port()])
}

func (s *sim) startOld(tag string) {
	s.running[s.mainName] = true
	s.images[s.mainName] = image + ":" + tag
	s.ports[appPort] = s.mainName
}

type fakeLocker struct {
	err      error
	wait     bool
	released bool
}

func (l *fakeLocker) Acquire(_ context.Context, onWait func()) (func(), error) {
	if l.wait {
		onWait()
	}
	if l.err != nil {
		return nil, l.err
	}
	return func() { l.released = true }, nil
}

type fakeRouter struct {
	syncs  int
	onSync func()
}

func (r *fakeRouter) Sync(context.Context) {
	r.syncs++
	if r.onSync != nil {
		r.onSync()
	}
}

type capturePusher struct {
	samples []obs.Sample
}

func (c *capturePusher) Push(_ context.Context, s []obs.Sample) {
	c.samples = append(c.samples, s...)
}

func (c *capturePusher) has(name string, labels obs.Labels) bool {
	_, ok := c.value(name, labels)
	return ok
}

func (c *capturePusher) value(name string, labels obs.Labels) (float64, bool) {
	for _, s := range c.samples {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			match = match && s.Labels[k] == v
		}
		if match {
			return s.Value, true
		}
	}
	return 0, false
}

type fakeVerifier struct {
	err   error
	wants []oidc.Expect
}

func (v *fakeVerifier) Verify(_ context.Context, token string, want oidc.Expect) (oidc.Claims, error) {
	v.wants = append(v.wants, want)
	if v.err != nil {
		return oidc.Claims{}, v.err
	}
	if token != goodOIDC {
		return oidc.Claims{}, oidc.ErrToken
	}
	return oidc.Claims{Repository: want.Repository, JobWorkflowRef: want.WorkflowRef, RunID: "7"}, nil
}

type env struct {
	t       *testing.T
	ident   *fakeVerifier
	bare    bool
	hubDir  string
	appDir  string
	sim     *sim
	exec    *procexectest.Fake
	svc     *Service
	out     *bytes.Buffer
	err     *bytes.Buffer
	logs    *bytes.Buffer
	locker  *fakeLocker
	router  *fakeRouter
	metrics *capturePusher
	slept   []time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, hubDir: filepath.Join(root, "hub"), appDir: filepath.Join(root, appName), sim: newSim(),
		out: &bytes.Buffer{}, err: &bytes.Buffer{}, logs: &bytes.Buffer{}, locker: &fakeLocker{}, metrics: &capturePusher{}}
	for _, d := range []string{e.hubDir, e.appDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	e.router = &fakeRouter{}
	e.write(filepath.Join(e.hubDir, "apps.list"), appName+" "+e.appDir+"\n")
	e.write(filepath.Join(e.appDir, "docker-compose.yml"), "services: {}\n")
	e.conf(registry.AppConf{Profile: registry.ProfileService, Port: appPort, HealthPath: healthPath, Image: image, Repo: appRepo})
	e.ident = &fakeVerifier{}
	e.exec = &procexectest.Fake{Handler: e.sim.handle}
	d := docker.Client{Exec: e.exec}
	noSleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	now := clock
	e.svc = &Service{
		Paths:       hub.Paths{Dir: e.hubDir},
		Apps:        registry.Apps{Path: filepath.Join(e.hubDir, "apps.list")},
		Journal:     journal.Journal{Path: filepath.Join(e.hubDir, "deploys.log"), Now: func() time.Time { return now }},
		Docker:      d,
		Gate:        health.Gate{Docker: d, HTTP: e.sim, Timeout: 2 * health.Interval, Interval: health.Interval, Sleep: noSleep},
		HTTP:        e.sim,
		Router:      e.router,
		Locker:      e.locker,
		CacheLocker: e.locker,
		LockWait:    time.Minute,
		Listening:   func(int) bool { return false },
		Now:         func() time.Time { now = now.Add(time.Second); return now },
		Sleep:       func(ctx context.Context, d time.Duration) error { e.slept = append(e.slept, d); return ctx.Err() },
		Out:         console.New(e.out),
		Err:         console.New(e.err),
		Log:         slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:     e.metrics,
		Identity:    e.ident,
		Audience:    "deploy-hub",
		WorkflowRef: hubRef,
	}
	return e
}

func identity() string {
	return "oidc=" + goodOIDC + "\ndigest=" + digest + "\n"
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) conf(c registry.AppConf) {
	e.t.Helper()
	if c.Repo == "" {
		c.Repo = appRepo
	}
	if err := c.Save(e.appDir); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) deployedBefore(tags ...string) {
	e.t.Helper()
	j := journal.Journal{Path: filepath.Join(e.hubDir, "deploys.log"), Now: func() time.Time { return clock }}
	for _, tag := range tags {
		if err := j.Append(journal.Entry{App: appName, Sha7: registry.Sha7(tag), Action: journal.ActionDeploy, Result: journal.ResultOK, Seconds: 1}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) state(s registry.DeployState) {
	e.t.Helper()
	if err := s.Save(e.appDir); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) dispatch(raw, stdin string) int {
	if !e.bare && slices.ContainsFunc([]string{VerbDeploy, VerbReport, VerbCacheGet, VerbCachePut}, func(v string) bool { return strings.HasPrefix(raw, v+" ") }) {
		stdin = identity() + stdin
	}
	return e.svc.Dispatch(context.Background(), raw, strings.NewReader(stdin))
}

func (e *env) journal() string {
	data, err := os.ReadFile(filepath.Join(e.hubDir, "deploys.log"))
	if err != nil {
		return ""
	}
	return string(data)
}

func (e *env) expectJournal(parts ...string) {
	e.t.Helper()
	j := e.journal()
	for _, p := range parts {
		if !strings.Contains(j, p) {
			e.t.Fatalf("journal lacks %q:\n%s", p, j)
		}
	}
}

func (e *env) expectLog(msgs ...string) {
	e.t.Helper()
	for _, m := range msgs {
		if !strings.Contains(e.logs.String(), `"msg":"`+m+`"`) {
			e.t.Fatalf("log lacks %q:\n%s", m, e.logs.String())
		}
	}
}

func (e *env) expectOut(w *bytes.Buffer, parts ...string) {
	e.t.Helper()
	for _, p := range parts {
		if !strings.Contains(w.String(), p) {
			e.t.Fatalf("output lacks %q:\n%s", p, w.String())
		}
	}
}

func (e *env) noSecretLeak(secret string) {
	e.t.Helper()
	for name, s := range map[string]string{"journal": e.journal(), "stdout": e.out.String(), "stderr": e.err.String(), "logs": e.logs.String()} {
		if strings.Contains(s, secret) {
			e.t.Fatalf("%s leaks the secret", name)
		}
	}
}
