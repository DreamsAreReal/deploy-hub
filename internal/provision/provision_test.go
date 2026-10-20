package provision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
	"go.yaml.in/yaml/v3"
)

var errSim = errors.New("simulated")

const (
	healthPath = "/health"
	botMem     = "128m"
	freshApp   = "fresh"
	webImage   = "ghcr.io/owner/web"
)

const webRepo = "owner/web"

func service() Request {
	return Request{App: "web", Profile: registry.ProfileService, Port: "9001", CPort: "8080", Mem: "256m", HealthPath: healthPath, Image: webImage, Repo: webRepo}
}

func TestRequestValidate(t *testing.T) {
	if err := service().Validate(); err != nil {
		t.Fatal(err)
	}
	bot := Request{App: "bot", Profile: registry.ProfileBot, Mem: botMem, HealthPath: "/", Image: "ghcr.io/owner/bot", Repo: "owner/bot"}
	if err := bot.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Request){
		func(r *Request) { r.Profile = "root" },
		func(r *Request) { r.App = "../x" },
		func(r *Request) { r.Port = "1;id" },
		func(r *Request) { r.CPort = "x" },
		func(r *Request) { r.Mem = "1t" },
		func(r *Request) { r.HealthPath = "/$(reboot)" },
		func(r *Request) { r.Image = "docker.io/evil" },
		func(r *Request) { r.Port = "" },
		func(r *Request) { r.Repo = "" },
		func(r *Request) { r.Repo = "owner/web;id" },
	}
	for i, mutate := range bad {
		r := service()
		mutate(&r)
		err := r.Validate()
		if !errors.Is(err, validate.ErrInvalid) && !errors.Is(err, ErrBadProfile) && !errors.Is(err, ErrPortNeeded) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
}

func TestArgs(t *testing.T) {
	testutil.Equal(t, strings.Join(service().Args(), " "), "--app web --profile service --port 9001 --cport 8080 --mem 256m --health-path /health --image ghcr.io/owner/web --repo owner/web")
	bot := Request{App: "b", Profile: registry.ProfileBot, Mem: botMem}
	testutil.Equal(t, strings.Join(bot.Args(), " "), "--app b --profile bot --mem 128m")
}

func TestRenderCompose(t *testing.T) {
	s := string(RenderCompose(service()))
	var parsed map[string]any
	testutil.NoErr(t, yaml.Unmarshal([]byte(s), &parsed))
	for _, want := range []string{"container_name: web", "mem_limit: 256m", `- "127.0.0.1:9001:8080"`, "env_file: .env", "restart: unless-stopped", "max-size: \"10m\""} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	r := service()
	r.Profile = registry.ProfileBot
	if data := RenderCompose(r); strings.Contains(string(data), "ports:") {
		t.Fatalf("bot must have no ports:\n%s", data)
	}
}

type fixture struct {
	root    string
	apps    registry.Apps
	out     *bytes.Buffer
	owned   []string
	syncs   int
	syncErr error
	metrics *pusher
	logs    bytes.Buffer
}

type pusher struct{ samples []obs.Sample }

func (p *pusher) Push(_ context.Context, s []obs.Sample) { p.samples = append(p.samples, s...) }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{root: root, apps: registry.Apps{Path: filepath.Join(root, "hub", "apps.list"), Root: root}, out: &bytes.Buffer{}, metrics: &pusher{}}
	if err := os.MkdirAll(filepath.Dir(f.apps.Path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.apps.Path, []byte("web "+filepath.Join(root, "web")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) provisioner() Provisioner {
	return Provisioner{
		Apps:    f.apps,
		Owner:   func(p string) error { f.owned = append(f.owned, p); return nil },
		Sync:    func(context.Context) error { f.syncs++; return f.syncErr },
		Out:     console.New(f.out),
		Log:     slog.New(slog.NewJSONHandler(&f.logs, nil)),
		Metrics: f.metrics,
	}
}

func TestProvisionCreatesEverythingOnceAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	r := service()
	r.App = "api"
	if err := f.provisioner().Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, "api")
	if !strings.Contains(f.out.String(), "apps.list: added 'api "+dir+"'") {
		t.Fatalf("out: %s", f.out)
	}
	if e, err := f.apps.Lookup("api"); err != nil || e.Dir != dir {
		t.Fatalf("lookup %+v %v", e, err)
	}
	testutil.Contains(t, f.out.String(), "  "+dir+": created\n", "  "+filepath.Join(dir, "docker-compose.yml")+": written\n",
		"  "+filepath.Join(dir, ".env")+": created (600, empty)\n")
	testutil.Equal(t, f.metrics.samples[0].Value, 5.0)
	f.out.Reset()
	f.metrics.samples = nil
	if err := f.provisioner().Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.out.String(), "added") {
		t.Fatal("registration must be idempotent")
	}
	testutil.Equal(t, f.metrics.samples[0].Value, 0.0)
}

func TestProvisionRegisteredApp(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "web")
	testutil.NoErr(t, f.provisioner().Run(context.Background(), service()))
	for _, name := range []string{"docker-compose.yml", "app.conf", ".env"} {
		testutil.Perm(t, filepath.Join(dir, name), filePerm)
	}
	conf, err := registry.LoadAppConf(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, conf, registry.AppConf{Profile: "service", Port: "9001", HealthPath: healthPath, Image: webImage, Repo: webRepo})
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, "docker-compose.yml")), string(RenderCompose(service())))
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, ".env")), "")
	testutil.Equal(t, len(f.owned), 4)
	testutil.Equal(t, f.syncs, 1)
	testutil.Contains(t, f.out.String(), "provision: ok (web -> "+dir+")")
	testutil.Equal(t, f.metrics.samples[0].Value, 4.0)
}

func TestProvisionKeepsExistingFiles(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "web")
	testutil.NoErr(t, f.provisioner().Run(context.Background(), service()))
	f.out.Reset()
	f.owned = nil
	testutil.Write(t, filepath.Join(dir, ".env"), "KEEP=1\n")
	testutil.NoErr(t, f.provisioner().Run(context.Background(), service()))
	testutil.Equal(t, len(f.owned), 0)
	testutil.Excludes(t, f.out.String(), "written")
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, ".env")), "KEEP=1\n")
}

func TestProvisionFailures(t *testing.T) {
	f := newFixture(t)
	if err := f.provisioner().Run(context.Background(), Request{App: "x"}); !errors.Is(err, ErrBadProfile) {
		t.Fatalf("validation: %v", err)
	}
	f.syncErr = errSim
	if err := f.provisioner().Run(context.Background(), service()); err != nil {
		t.Fatalf("sync failure is not fatal: %v", err)
	}
	if !strings.Contains(f.out.String(), msgSyncSkipped) {
		t.Fatal("sync failure must be reported")
	}
	testutil.Contains(t, f.logs.String(), `"msg":"caddy-sync after provision failed","app":"web"`, `"msg":"app provisioned","app":"web"`)

	f2 := newFixture(t)
	p := f2.provisioner()
	p.Owner = func(string) error { return errSim }
	if err := p.Run(context.Background(), service()); !errors.Is(err, errSim) {
		t.Fatalf("owner error: %v", err)
	}

	f3 := newFixture(t)
	if err := os.WriteFile(filepath.Join(f3.root, "web"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f3.provisioner().Run(context.Background(), service()); err == nil {
		t.Fatal("app dir blocked by a file must fail")
	}

	f4 := newFixture(t)
	p4 := f4.provisioner()
	p4.Apps = registry.Apps{Path: filepath.Join(f4.root, "missing", "apps.list"), Root: f4.root}
	r := service()
	r.App = freshApp
	if err := p4.Run(context.Background(), r); err == nil {
		t.Fatal("unreadable apps.list must fail")
	}
}

func TestProvisionAppendFails(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, os.Remove(f.apps.Path))
	testutil.Mkdir(t, f.apps.Path)
	r := service()
	r.App = freshApp
	testutil.Fails(t, f.provisioner().Run(context.Background(), r))
}

func TestEnsureStatError(t *testing.T) {
	f := newFixture(t)
	blocker := filepath.Join(f.root, "blocker")
	testutil.Write(t, blocker, "")
	var created []string
	testutil.Fails(t, f.provisioner().ensure(step{path: filepath.Join(blocker, "\x00bad"), create: func(string) error { return nil }}, &created))
	testutil.Equal(t, len(created), 0)
}
