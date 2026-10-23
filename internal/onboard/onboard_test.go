package onboard

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
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/provision"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errSim = errors.New("simulated")

const (
	vpsAddr       = "192.0.2.10"
	hostKeyLine   = vpsAddr + " ssh-ed25519 AAAAtestkey"
	allRepoKeys   = "VPS_SSH_KEY VPS_HOST VPS_HOST_KEY"
	appWeb        = "web"
	optWeb        = "/opt/web"
	failureOutput = "boom"
	stepCheckout  = "checkout"
	flagPort      = "--port"
	flagDocker    = "--dockerfile"
	flagCompose   = "--compose"
	dockerDir     = "docker/Dockerfile"
	alpine        = "FROM alpine\n"
	flagDryRun    = "--dry-run"
	flagProfile   = "--profile"
)

const (
	deployKeyContent = "fake-deploy-key-material-for-tests\n"
	repoNotFound     = "repo not found on GitHub: DreamsAreReal/web"
)

type world struct {
	t          *testing.T
	repo       map[string]string
	noRepo     bool
	noBranch   bool
	facts      provision.Facts
	secrets    string
	pushFails  bool
	failOn     string
	exec       *procexectest.Fake
	out        *bytes.Buffer
	err        *bytes.Buffer
	keyPath    string
	secretSet  map[string]string
	noHostKey  bool
	provisions []string
	logs       bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, repo: map[string]string{"Dockerfile": "FROM nginx\nEXPOSE 8080\n"}, out: &bytes.Buffer{}, err: &bytes.Buffer{},
		facts: provision.Facts{Dir: optWeb, PortsTaken: []int{22, 9001, 9002}}, secretSet: map[string]string{}}
	w.keyPath = filepath.Join(t.TempDir(), "deploy_hub_key")
	if err := os.WriteFile(w.keyPath, []byte(deployKeyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	w.exec = &procexectest.Fake{Handler: w.handle}
	return w
}

type route struct {
	match   func(line string) bool
	respond func(w *world, c procexec.Command) (procexec.Result, error)
}

func prefix(p string) func(string) bool {
	return func(l string) bool { return strings.HasPrefix(l, p) }
}
func substr(p string) func(string) bool { return func(l string) bool { return strings.Contains(l, p) } }

var routes = []route{
	{prefix("gh repo view"), func(w *world, _ procexec.Command) (procexec.Result, error) {
		if w.noRepo {
			return procexec.Result{}, errSim
		}
		if w.noBranch {
			return procexec.Result{Stdout: "\n"}, nil
		}
		return procexec.Result{Stdout: "main\n"}, nil
	}},
	{prefix("git clone"), func(w *world, c procexec.Command) (procexec.Result, error) {
		return procexec.Result{}, w.clone(c.Args[len(c.Args)-1])
	}},
	{substr("deploy-hub facts"), func(w *world, _ procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: w.facts.Encode()}, nil
	}},
	{substr("deploy-hub provision"), func(w *world, c procexec.Command) (procexec.Result, error) {
		w.provisions = append(w.provisions, procexectest.Line(c))
		return procexec.Result{Stdout: "provision: ok\n"}, nil
	}},
	{prefix("gh secret list"), func(w *world, _ procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: w.secrets}, nil
	}},
	{prefix("gh secret set"), func(w *world, c procexec.Command) (procexec.Result, error) {
		w.secretSet[c.Args[2]] = c.Stdin
		return procexec.Result{}, nil
	}},
	{prefix("ssh-keygen -F"), func(w *world, _ procexec.Command) (procexec.Result, error) {
		if w.noHostKey {
			return procexec.Result{}, errSim
		}
		return procexec.Result{Stdout: "# Host " + vpsAddr + " found: line 3\n\n" + hostKeyLine + "\n"}, nil
	}},
	{substr(" push -q origin main"), func(w *world, _ procexec.Command) (procexec.Result, error) {
		if w.pushFails {
			return procexec.Result{}, errSim
		}
		return procexec.Result{}, nil
	}},
	{prefix("ssh -i"), func(*world, procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: "app | sha | health | last deploy\n", Stderr: "warn\n"}, nil
	}},
}

func (w *world) handle(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if w.failOn != "" && strings.Contains(line, w.failOn) {
		return procexec.Result{Stderr: failureOutput}, errSim
	}
	for _, r := range routes {
		if r.match(line) {
			return r.respond(w, c)
		}
	}
	return procexec.Result{}, nil
}

func (w *world) clone(dest string) error {
	for name, content := range w.repo {
		p := filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (w *world) run(args ...string) int {
	o := Onboarder{
		Config: Config{HostAlias: "vpn", Addr: vpsAddr, DeployKey: w.keyPath, HubRepo: "DreamsAreReal/deploy-hub",
			Owner: "DreamsAreReal", RemoteBinary: "/opt/deploy-hub/bin/deploy-hub", DeployUser: "deploy"},
		Exec: w.exec, Out: console.New(w.out), Err: console.New(w.err), Log: slog.New(slog.NewJSONHandler(&w.logs, nil)),
	}
	return o.Run(context.Background(), args)
}

func (w *world) expect(buf *bytes.Buffer, parts ...string) {
	w.t.Helper()
	for _, p := range parts {
		if !strings.Contains(buf.String(), p) {
			w.t.Fatalf("output lacks %q:\n%s", p, buf.String())
		}
	}
}

func TestDryRunPrintsPlanAndChangesNothing(t *testing.T) {
	w := newWorld(t)
	if code := w.run(appWeb, flagDryRun); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, w.err)
	}
	testutil.True(t, strings.HasPrefix(w.out.String(), "onboard: DreamsAreReal/web -> app 'web'\n"+msgGathering+"\n"), w.out.String())
	w.expect(w.out,
		"\n"+msgPlan+"\n",
		"auto-detected: service (EXPOSE 8080 in Dockerfile)",
		"commit caller stub to DreamsAreReal/web@main (missing)",
		"set secret VPS_SSH_KEY on DreamsAreReal/web",
		"set secret VPS_HOST on DreamsAreReal/web",
		"set secret VPS_HOST_KEY on DreamsAreReal/web",
		"write /opt/web/docker-compose.yml (image ghcr.io/dreamsarereal/web, 127.0.0.1:9003->8080, mem 256m)",
		"write /opt/web/app.conf (profile=service port=9003 health_path=/)",
		"allowlist: add 'web /opt/web' to apps.list",
		"create /opt/web on the VPS",
		"create empty /opt/web/.env (600, owner deploy)",
		msgDry,
	)
	testutil.Excludes(t, w.out.String(), msgApplying)
	if w.exec.Count("secret set") != 0 || len(w.provisions) != 0 || w.exec.Count("push") != 0 {
		t.Fatalf("dry run changed something: %v", w.exec.Lines())
	}
}

func TestApplyOnboardsEndToEnd(t *testing.T) {
	w := newWorld(t)
	w.repo[".env.example"] = "API_KEY=\nDB_URL=x\n# c\nAPI_KEY=dup\n"
	if code := w.run("Owner/Web", "--app", appWeb, flagPort, "9100"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, w.err)
	}
	testutil.Equal(t, w.secretSet, map[string]string{deployKeyName: deployKeyContent, repoVarHost: vpsAddr, repoVarHostKey: hostKeyLine})
	for _, l := range w.exec.Lines() {
		if strings.Contains(l, "fake-deploy-key-material") {
			t.Fatal("key leaked into argv")
		}
	}
	if len(w.provisions) != 1 || !strings.Contains(w.provisions[0], "ssh vpn /opt/deploy-hub/bin/deploy-hub provision --app web --profile service --port 9100 --cport 8080 --mem 256m --health-path / --image ghcr.io/owner/web") {
		t.Fatalf("provision: %v", w.provisions)
	}
	if w.exec.Count("commit -q -m ci: deploy to VPS via deploy-hub") != 1 || w.exec.Count("push -q origin main") != 1 {
		t.Fatalf("stub commit: %v", w.exec.Lines())
	}
	w.expect(&w.logs, `"msg":"onboard plan","repo":"Owner/Web","app":"web"`, `"msg":"repo secret set"`, `"msg":"onboard applied"`)
	w.expect(w.out, "secret VPS_SSH_KEY: set", "secret VPS_HOST: set", "secret VPS_HOST_KEY: set", "stub: committed to main", "done.", "paste the app secrets into /opt/web/.env", "variables found in the repo: API_KEY DB_URL")
}

func TestMissingHostKeyStopsBeforeChanges(t *testing.T) {
	w := newWorld(t)
	w.noHostKey = true
	testutil.Equal(t, w.run(appWeb), ExitFail)
	w.expect(w.err, "no known_hosts entry for "+vpsAddr)
	w.expect(&w.logs, `"msg":"onboard failed"`, `"msg":"command failed","binary":"ssh-keygen"`)
	testutil.Equal(t, len(w.provisions), 0)
	testutil.Equal(t, w.secretSet, map[string]string{deployKeyName: deployKeyContent, repoVarHost: vpsAddr})
}

func TestStubContent(t *testing.T) {
	want := "name: deploy\non:\n  push: {branches: [main]}\npermissions: {}\nconcurrency: {group: deploy, cancel-in-progress: false}\njobs:\n  deploy:\n    name: deploy\n" +
		"    permissions: {contents: read, packages: write, id-token: write}\n    uses: H/R/.github/workflows/deploy.yml@main\n    with: {app: web, dockerfile: docker/Dockerfile}\n" +
		`    secrets: {VPS_SSH_KEY: "${{ secrets.VPS_SSH_KEY }}", VPS_HOST: "${{ secrets.VPS_HOST }}", VPS_HOST_KEY: "${{ secrets.VPS_HOST_KEY }}"}`
	if got := Stub("main", "H/R", Options{App: appWeb, Dockerfile: dockerDir}); got != want {
		t.Fatalf("got\n%s", got)
	}
	testutil.Equal(t, Stub("main", "H/R", Options{App: appWeb, Dockerfile: defaultDockerfile}), strings.Replace(want, ", dockerfile: docker/Dockerfile", "", 1))
}

func TestProtectedBranchOpensPR(t *testing.T) {
	w := newWorld(t)
	w.pushFails = true
	w.secrets = allRepoKeys
	if code := w.run(appWeb, flagProfile, "static"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, w.err)
	}
	w.expect(w.out, "onboard: DreamsAreReal/web -> app 'web' (profile static)\n")
	if w.exec.Count("checkout -q -b deploy-hub-onboard") != 1 || w.exec.Count("gh pr create") != 1 {
		t.Fatalf("PR flow: %v", w.exec.Lines())
	}
	w.expect(w.out, "default branch is protected", msgLeftNone)
	if w.exec.Count("secret set") != 0 {
		t.Fatal("existing secret must not be overwritten")
	}
}

func TestAlreadyOnboardedReportsNoChanges(t *testing.T) {
	w := newWorld(t)
	w.secrets = allRepoKeys + " OTHER"
	w.facts = provision.Facts{Dir: optWeb, InList: true, DirExists: true, ConfExists: true, ComposeExists: true, EnvExists: true, Port: "9001"}
	w.repo[".github/workflows/deploy.yml"] = Stub("main", "DreamsAreReal/deploy-hub", Options{App: appWeb, Dockerfile: defaultDockerfile}) + "\n"
	if code := w.run(appWeb); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, w.err)
	}
	w.expect(w.out, msgNoChanges)
	if len(w.provisions) != 0 {
		t.Fatal("nothing to apply")
	}
}

func TestOutdatedHubStubIsRegenerated(t *testing.T) {
	w := newWorld(t)
	w.secrets = allRepoKeys
	w.facts = provision.Facts{Dir: optWeb, InList: true, DirExists: true, ConfExists: true, ComposeExists: true, EnvExists: true, Port: "9001"}
	w.repo[".github/workflows/deploy.yml"] = "jobs:\n  deploy:\n    uses: DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@v1\n"
	if code := w.run(appWeb); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	w.expect(w.out, "(differs)", "stub: committed to main")
	if len(w.provisions) != 1 {
		t.Fatal("provision runs idempotently on apply")
	}
}

func TestOwnCIIsNotOverwritten(t *testing.T) {
	w := newWorld(t)
	w.repo[".github/workflows/deploy.yml"] = "jobs:\n  test: {runs-on: ubuntu-latest}\n"
	if code := w.run(appWeb); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	w.expect(w.err, "already has a non-hub deploy.yml")
}

func TestProfileDetection(t *testing.T) {
	w := newWorld(t)
	w.repo["Dockerfile"] = alpine
	w.repo["docker-compose.yml"] = "services:\n  bot:\n    build: .\n    ports: ['9000:3000']\n    environment: {TOKEN: '${BOT_TOKEN:?}', X: '${OTHER}'}\n"
	if code := w.run(appWeb, flagDryRun); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	w.expect(w.out, "auto-detected: service (published port in compose)", "127.0.0.1:9003->3000")

	w2 := newWorld(t)
	w2.repo["Dockerfile"] = alpine
	if code := w2.run(appWeb); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	w2.expect(w2.out, "auto-detected: bot (no EXPOSE / no published port)", "(no .env.example/compose vars found",
		"  write /opt/web/docker-compose.yml (image ghcr.io/dreamsarereal/web, mem 128m)\n", "  write /opt/web/app.conf (profile=bot health_path=/)\n", msgApplying+"\n")
	if !strings.Contains(w2.provisions[0], "--profile bot --mem 128m") || strings.Contains(w2.provisions[0], flagPort) {
		t.Fatalf("bot provision: %v", w2.provisions)
	}

	w3 := newWorld(t)
	w3.repo["docker-compose.yml"] = "services:\n  a:\n    build: .\n    environment: {B: '${ZED}', A: '${ALPHA:-x}'}\n"
	w3.run(appWeb, flagProfile, "service", "--cport", "5000", "--mem", "512m")
	w3.expect(w3.out, "variables found in the repo: ALPHA ZED", "127.0.0.1:9003->5000, mem 512m")
}

func TestExistingPortIsKept(t *testing.T) {
	w := newWorld(t)
	w.facts.ConfExists, w.facts.Port = true, "9050"
	w.run(appWeb, flagDryRun, flagPort, "9001")
	w.expect(w.out, "127.0.0.1:9050->8080")
}

func TestGatherFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*world)
		args  []string
		msg   string
	}{
		{"no repo", func(w *world) { w.noRepo = true }, []string{appWeb}, repoNotFound},
		{"no default branch", func(w *world) { w.noBranch = true }, []string{appWeb}, repoNotFound},
		{"no dockerfile", func(w *world) { delete(w.repo, "Dockerfile") }, []string{appWeb}, "the repo has no Dockerfile (Dockerfile)"},
		{"custom dockerfile missing", func(*world) {}, []string{appWeb, flagDocker, dockerDir}, "the repo has no Dockerfile (docker/Dockerfile)"},
		{"named compose missing", func(*world) {}, []string{appWeb, flagCompose, sttCompose}, "the repo has no compose file (stt/docker-compose.yml)"},
		{"multi build", func(w *world) {
			w.repo["compose.yml"] = "services:\n  a: {build: .}\n  b: {build: ./b}\n"
		}, []string{appWeb}, "builds more than one service"},
		{"port taken", func(*world) {}, []string{appWeb, flagPort, "9001"}, "port already in use on the VPS: 9001"},
		{"clone fails", func(w *world) { w.failOn = "git clone" }, []string{appWeb}, "simulated"},
		{"facts fail", func(w *world) { w.failOn = "facts" }, []string{appWeb}, failureOutput},
	}
	for _, c := range cases {
		w := newWorld(t)
		c.setup(w)
		if code := w.run(c.args...); code != ExitFail {
			t.Fatalf("%s: exit %d", c.name, code)
		}
		w.expect(w.err, c.msg)
	}
}

func TestApplyFailures(t *testing.T) {
	steps := []struct {
		name      string
		protected bool
	}{{"secret set", false}, {"provision", false}, {"git -C", false}, {"commit", false}, {stepCheckout, true}, {"push -q -f", true}, {"pr create", true}}
	for _, step := range steps {
		w := newWorld(t)
		w.failOn = step.name
		w.pushFails = step.protected
		if code := w.run(appWeb); code != ExitFail {
			t.Fatalf("%s: exit %d", step.name, code)
		}
	}
	w := newWorld(t)
	w.keyPath = filepath.Join(t.TempDir(), "missing")
	if code := w.run(appWeb); code != ExitFail {
		t.Fatal("missing key must fail")
	}
	w.expect(w.err, "deploy key not found")
	w2 := newWorld(t)
	w2.repo[".github"] = "file blocks the workflows dir"
	if code := w2.run(appWeb); code != ExitFail {
		t.Fatal("stub dir creation failure must surface")
	}
	w3 := newWorld(t)
	w3.repo[".github/workflows/deploy.yml/x"] = "dir in place of the stub"
	w3.repo[".github/workflows/deploy.yml/uses: DreamsAreReal/deploy-hub/"] = ""
	if code := w3.run(appWeb); code != ExitFail {
		t.Fatal("stub write failure must surface")
	}
}

func TestProxyAndHelpAndUsage(t *testing.T) {
	w := newWorld(t)
	if code := w.run("status"); code != ExitOK {
		t.Fatalf("status exit %d", code)
	}
	if got := w.exec.Lines()[0]; got != "ssh -i "+w.keyPath+" -o IdentitiesOnly=yes deploy@"+vpsAddr+" status" {
		t.Fatalf("proxy: %s", got)
	}
	w.expect(w.out, "app | sha")
	w.expect(w.err, "warn")
	w.failOn = "ssh -i"
	if code := w.run("history", appWeb); code != ExitFail {
		t.Fatal("proxy failure must propagate")
	}
	if code := w.run("--help"); code != ExitOK {
		t.Fatal("help")
	}
	w.expect(w.out, "usage: deploy-hub onboard")
	for _, args := range [][]string{{}, {"--bogus"}, {appWeb, flagPort}, {appWeb, flagProfile, "root"}, {appWeb, "--app", "Bad Name"}, {appWeb, flagDocker, "../../etc/passwd"}, {appWeb, flagCompose, "/etc/compose.yml"}, {appWeb, "--mem", "lots"}} {
		if code := w.run(args...); code != ExitFail {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestTempDirFailure(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	w := newWorld(t)
	if code := w.run(appWeb); code != ExitFail {
		t.Fatal("temp dir failure must surface")
	}
}

const (
	appSTT     = "stt"
	sttCompose = "stt/docker-compose.yml"
	sttStub    = ".github/workflows/deploy-stt.yml"
)

func TestSecondAppOfARepoGetsItsOwnStub(t *testing.T) {
	want := "name: deploy-stt\non:\n  push: {branches: [main]}\npermissions: {}\nconcurrency: {group: deploy-stt, cancel-in-progress: false}\njobs:\n  deploy:\n    name: deploy\n" +
		"    permissions: {contents: read, packages: write, id-token: write}\n    uses: H/R/.github/workflows/deploy.yml@main\n" +
		"    with: {app: stt, dockerfile: stt/Dockerfile, compose: stt/docker-compose.yml}\n" +
		`    secrets: {VPS_SSH_KEY: "${{ secrets.VPS_SSH_KEY }}", VPS_HOST: "${{ secrets.VPS_HOST }}", VPS_HOST_KEY: "${{ secrets.VPS_HOST_KEY }}"}`
	stt := Options{App: appSTT, Dockerfile: "stt/Dockerfile", Compose: sttCompose}
	testutil.Equal(t, Stub("main", "H/R", stt), want)
	testutil.Equal(t, StubPath(stt), sttStub)
	testutil.Equal(t, StubPath(Options{App: appWeb}), ".github/workflows/deploy.yml")
}

func TestComposeOptionDrivesDetectionAndKeepsTheFirstAppStub(t *testing.T) {
	w := newWorld(t)
	first := Stub("main", "DreamsAreReal/deploy-hub", Options{App: appWeb, Dockerfile: defaultDockerfile}) + "\n"
	w.repo[".github/workflows/deploy.yml"] = first
	w.repo["Dockerfile"] = alpine
	w.repo["docker-compose.yml"] = "services:\n  web:\n    build: .\n    ports: ['9000:3000']\n    environment: {A: '${ROOT_ONLY}'}\n"
	w.repo["stt/Dockerfile"] = alpine
	w.repo[sttCompose] = "services:\n  app:\n    build: {context: ., dockerfile: stt/Dockerfile}\n    ports: ['9003:8790']\n    environment: {T: '${STT_TOKENS}'}\n"
	if code := w.run(appWeb, "--app", appSTT, flagDocker, "stt/Dockerfile", flagCompose, sttCompose); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, w.err)
	}
	w.expect(w.out, "auto-detected: service (published port in compose)", "127.0.0.1:9003->8790",
		"commit caller stub to DreamsAreReal/web@main (missing)", "variables found in the repo: STT_TOKENS")
	testutil.Excludes(t, w.out.String(), "ROOT_ONLY")
	if w.exec.Count("add "+sttStub) != 1 {
		t.Fatalf("stub must go to its own file: %v", w.exec.Lines())
	}
}
