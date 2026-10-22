package docker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errBoom = errors.New("boom")

const (
	composeFile = "/a/dc.yml"
	standbyName = "a-bg"
	imageRef    = "img:t"
)

func client(h procexectest.Handler) (Client, *procexectest.Fake) {
	f := &procexectest.Fake{Handler: h}
	return Client{Exec: f}, f
}

func reply(stdout string, err error) procexectest.Handler {
	return func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stdout: stdout}, err }
}

func TestLoginLogoutPull(t *testing.T) {
	c, f := client(reply("", nil))
	ctx := context.Background()
	if err := c.Login(ctx, "ghcr.io", "actor", "tok"); err != nil {
		t.Fatal(err)
	}
	testutil.NoErr(t, c.Logout(ctx, "ghcr.io"))
	if _, err := c.Pull(ctx, "img:sha-1"); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if calls[0].Stdin != "tok" || strings.Contains(procexectest.Line(calls[0]), "tok") {
		t.Fatal("token must go through stdin only")
	}
	want := []string{"docker login ghcr.io -u actor --password-stdin", "docker logout ghcr.io", "docker pull -q img:sha-1"}
	if !reflect.DeepEqual(f.Lines(), want) {
		t.Fatalf("got %v", f.Lines())
	}
}

func TestPullReturnsCombinedOutput(t *testing.T) {
	c, _ := client(func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: "o", Stderr: "denied\n"}, errBoom
	})
	out, err := c.Pull(context.Background(), "x")
	if !errors.Is(err, errBoom) || out != "odenied" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestComposeCommands(t *testing.T) {
	c, f := client(reply("", nil))
	ctx := context.Background()
	if err := c.ComposeUp(ctx, UpOptions{File: composeFile, Image: imageRef, Pull: PullNever, Services: []string{MainService}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ComposeUp(ctx, UpOptions{File: composeFile, Image: imageRef, Pull: PullMissing, NoRecreate: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.ComposeStop(ctx, composeFile, imageRef, MainService); err != nil {
		t.Fatal(err)
	}
	if err := c.ComposeValidate(ctx, "/a", "dc.yml.new", imageRef); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker compose -f /a/dc.yml up -d --pull never app",
		"docker compose -f /a/dc.yml up -d --pull missing --no-recreate",
		"docker compose -f /a/dc.yml stop app",
		"docker compose -f dc.yml.new config -q",
	}
	if !reflect.DeepEqual(f.Lines(), want) {
		t.Fatalf("got %v", f.Lines())
	}
	for _, call := range f.Calls() {
		if !reflect.DeepEqual(call.Env, []string{"DEPLOY_IMAGE=img:t"}) {
			t.Fatalf("env: %v", call.Env)
		}
	}
	if f.Calls()[3].Dir != "/a" {
		t.Fatal("validate must run inside the app dir")
	}
}

func TestComposeContainer(t *testing.T) {
	c, f := client(reply("\nweb-1\nweb-2\n", nil))
	if got := c.ComposeContainer(context.Background(), composeFile, MainService); got != "web-1" {
		t.Fatalf("got %q", got)
	}
	standbyFirst, _ := client(reply("web-bg\ta=b,com.docker.compose.oneoff=True\nweb\ta=b,com.docker.compose.oneoff=False\n", nil))
	testutil.Equal(t, standbyFirst.ComposeContainer(context.Background(), composeFile, MainService), "web")
	onlyStandby, _ := client(reply("web-bg\tcom.docker.compose.oneoff=True\n", nil))
	testutil.Equal(t, onlyStandby.ComposeContainer(context.Background(), composeFile, MainService), "")
	if f.Calls()[0].Env[0] != "DEPLOY_IMAGE=placeholder" {
		t.Fatal("ps needs a placeholder image")
	}
	c2, _ := client(reply("web", errBoom))
	if got := c2.ComposeContainer(context.Background(), composeFile, MainService); got != "" {
		t.Fatalf("error must yield empty name, got %q", got)
	}
}

func inspectFake(cmd procexec.Command) (procexec.Result, error) {
	outputs := map[string]string{
		formatState:  "running\n",
		formatHealth: "healthy\n",
		formatPorts:  "443/tcp 9443\n80/tcp 9001\n",
	}
	return procexec.Result{Stdout: outputs[cmd.Args[2]]}, nil
}

func TestInspectHelpers(t *testing.T) {
	ctx := context.Background()
	c, _ := client(inspectFake)
	testutil.Equal(t, []string{c.State(ctx, "x"), c.HealthStatus(ctx, "x")}, []string{"running", "healthy"})
	testutil.Equal(t, c.ContainerPort(ctx, "x", "9001"), "80")
	testutil.Equal(t, c.ContainerPort(ctx, "x", "1234"), "")
}

func TestInspectDefaults(t *testing.T) {
	ctx := context.Background()
	bad, _ := client(reply("", errBoom))
	testutil.Equal(t, []string{bad.State(ctx, "x"), bad.HealthStatus(ctx, "x"), bad.ContainerPort(ctx, "x", "1"), bad.ImageLabel(ctx, "img:t", "k")}, []string{StateMissing, NoHealth, "", ""})
	empty, _ := client(reply("", nil))
	testutil.Equal(t, []string{empty.State(ctx, "x"), empty.HealthStatus(ctx, "x")}, []string{StateMissing, NoHealth})
}

func TestRunning(t *testing.T) {
	c, f := client(reply("abc123\n", nil))
	if !c.Running(context.Background(), "web") {
		t.Fatal("expected running")
	}
	if f.Lines()[0] != "docker ps -q -f name=^web$" {
		t.Fatalf("got %v", f.Lines())
	}
	for _, h := range []procexectest.Handler{reply("", nil), reply("abc", errBoom)} {
		c2, _ := client(h)
		if c2.Running(context.Background(), "web") {
			t.Fatal("expected not running")
		}
	}
}

func TestRunStandbyArgs(t *testing.T) {
	c, f := client(reply("", nil))
	ctx := context.Background()
	if err := c.RunStandby(ctx, Standby{Name: standbyName, File: composeFile, Service: MainService, Image: imageRef, HostPort: "19001", Port: "80"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RunStandby(ctx, Standby{Name: standbyName, File: composeFile, Service: "worker", Image: imageRef}); err != nil {
		t.Fatal(err)
	}
	testutil.NoErr(t, c.Stop(ctx, standbyName))
	want := []string{
		"docker compose -f " + composeFile + " run -d --no-deps --name a-bg -p 127.0.0.1:19001:80 app",
		"docker compose -f " + composeFile + " run -d --no-deps --name a-bg worker",
		"docker stop a-bg",
	}
	if !reflect.DeepEqual(f.Lines(), want) {
		t.Fatalf("got %v", f.Lines())
	}
	testutil.Equal(t, f.Calls()[0].Env, []string{"DEPLOY_IMAGE=" + imageRef})
}

func TestImageLabel(t *testing.T) {
	c, f := client(reply("false\n", nil))
	testutil.Equal(t, c.ImageLabel(context.Background(), imageRef, "deploy-hub.overlap"), "false")
	testutil.Equal(t, f.Lines()[0], `docker image inspect --format {{index .Config.Labels "deploy-hub.overlap"}} `+imageRef)
}

func TestImageCommands(t *testing.T) {
	c, f := client(reply("sha-1\nlatest\n", nil))
	ctx := context.Background()
	tags, err := c.ImageTags(ctx, "img")
	if err != nil || !reflect.DeepEqual(tags, []string{"sha-1", "latest"}) {
		t.Fatalf("tags %v %v", tags, err)
	}
	if err := c.RemoveImage(ctx, "img:sha-1"); err != nil {
		t.Fatal(err)
	}
	testutil.NoErr(t, c.Remove(ctx, standbyName))
	testutil.NoErr(t, c.PruneDangling(ctx))
	want := []string{"docker images img --format {{.Tag}}", "docker rmi img:sha-1", "docker rm -f a-bg", "docker image prune -f"}
	if !reflect.DeepEqual(f.Lines(), want) {
		t.Fatalf("got %v", f.Lines())
	}
	bad, _ := client(reply("", errBoom))
	if _, err := bad.ImageTags(ctx, "img"); !errors.Is(err, errBoom) {
		t.Fatal("tags error")
	}
	testutil.Is(t, bad.Logout(ctx, "ghcr.io"), errBoom)
	testutil.Is(t, bad.Remove(ctx, standbyName), errBoom)
	testutil.Is(t, bad.PruneDangling(ctx), errBoom)
}
