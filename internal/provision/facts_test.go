package provision

import (
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type factsFixture struct {
	root string
	web  string
	apps registry.Apps
	proc string
}

func newFactsFixture(t *testing.T) factsFixture {
	t.Helper()
	root := t.TempDir()
	f := factsFixture{root: root, web: filepath.Join(root, "web"), apps: registry.Apps{Path: filepath.Join(root, "deploy-hub", "apps.list"), Root: root}, proc: filepath.Join(root, "proc_tcp")}
	codex := filepath.Join(root, "elsewhere", "codex")
	testutil.Write(t, f.apps.Path, "web "+f.web+"\ncodeapp "+codex+"\n")
	testutil.Write(t, filepath.Join(f.web, "app.conf"), "profile=service\nport=9001\n")
	testutil.Write(t, filepath.Join(f.web, "docker-compose.yml"), "services: {}\n")
	testutil.Write(t, filepath.Join(codex, "app.conf"), "port=9003\n")
	testutil.Write(t, filepath.Join(root, "orphan", "app.conf"), "port=9002\n")
	testutil.Write(t, f.proc, "sl local rem st\n 0: 0100007F:1F90 00000000:0000 0A\n")
	return f
}

func TestGatherEncodeParse(t *testing.T) {
	fx := newFactsFixture(t)
	f := Gather(fx.apps, fx.root, "web", []string{fx.proc})
	want := Facts{Dir: fx.web, InList: true, DirExists: true, ConfExists: true, ComposeExists: true, Port: "9001", PortsTaken: []int{8080, 9001, 9002, 9003}}
	testutil.Equal(t, f, want)
	encoded := f.Encode()
	testutil.Equal(t, encoded, "dir="+fx.web+"\nin_list=1\ndir_exists=1\nconf_exists=1\ncompose_exists=1\nenv_exists=0\nport=9001\nports_taken=8080 9001 9002 9003\n")
	testutil.Equal(t, ParseFacts(encoded+"ports_taken=junk\n"), want)
}

func TestGatherUnknownApp(t *testing.T) {
	fx := newFactsFixture(t)
	fresh := Gather(fx.apps, fx.root, "new", nil)
	testutil.Equal(t, fresh, Facts{Dir: filepath.Join(fx.root, "new"), PortsTaken: []int{9001, 9002, 9003}})
	testutil.Equal(t, ParseFacts("ports_taken=1 x 2\n").PortsTaken, []int{1, 2})
}
