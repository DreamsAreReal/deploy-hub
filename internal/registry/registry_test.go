package registry

import (
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	codexDir = "/opt/codex"
	codeApp  = "codeapp"
	webApp   = "web"
	plainTag = "plain"
)

func TestParseApps(t *testing.T) {
	got := ParseApps("# comment\n\ntasks\ncodeapp /opt/codex extra\n  #indented\nweb\n", "/opt")
	testutil.Equal(t, got, []Entry{{"tasks", "/opt/tasks"}, {codeApp, codexDir}, {webApp, "/opt/web"}})
}

func TestAppsMissingFile(t *testing.T) {
	a := Apps{Path: filepath.Join(t.TempDir(), "apps.list")}
	_, err := a.All()
	testutil.Fails(t, err)
	_, err = a.Lookup("x")
	testutil.Fails(t, err)
	testutil.Equal(t, a.DirFor("x"), "/opt/x")
	testutil.Equal(t, (Apps{Path: a.Path, Root: "/srv"}).DirFor("new"), "/srv/new")
	testutil.Fails(t, (Apps{Path: filepath.Join(a.Path, "nested", "x")}).Add(Entry{Name: "a", Dir: "/b"}))
}

func TestAppsLookupAdd(t *testing.T) {
	a := Apps{Path: filepath.Join(t.TempDir(), "apps.list")}
	testutil.NoErr(t, a.Add(Entry{Name: codeApp, Dir: codexDir}))
	e, err := a.Lookup(codeApp)
	testutil.NoErr(t, err)
	testutil.Equal(t, e.Dir, codexDir)
	_, err = a.Lookup("other")
	testutil.Is(t, err, ErrUnknownApp)
	testutil.Equal(t, a.DirFor(codeApp), codexDir)
}

func TestKV(t *testing.T) {
	testutil.Equal(t, ParseKV("a=1\nb=x=y\na=2\nnoequals\n=empty\n"), map[string]string{"a": "1", "b": "x=y", "": "empty"})
	testutil.Equal(t, EncodeKV([]KV{{"a", "1"}, {"b", ""}, {"c", "3"}}), "a=1\nc=3\n")
	_, err := ReadKV(filepath.Join(t.TempDir(), "missing"))
	testutil.Fails(t, err)
	testutil.Fails(t, WriteFile(filepath.Join(t.TempDir(), "no", "dir"), "x"))
}

func TestAppConfRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := AppConf{Mode: ModeBYOC, Main: webApp, Port: "9001", HealthPath: "/", HMode: HealthHTTP, Image: "ghcr.io/o/a"}
	testutil.NoErr(t, c.Save(dir))
	path := filepath.Join(dir, AppConfFile)
	testutil.Equal(t, testutil.Read(t, path), "mode=byoc\nmain=web\nport=9001\nhealth_path=/\nhmode=http\nimage=ghcr.io/o/a\n")
	testutil.Perm(t, path, filePerm)
	got, err := LoadAppConf(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, got, c)
	testutil.True(t, got.IsBYOC() && got.HealthMode() == HealthHTTP, "byoc health mode")
	_, err = LoadAppConf(t.TempDir())
	testutil.Fails(t, err)
}

func TestHealthMode(t *testing.T) {
	cases := map[string]string{ProfileStatic: HealthHTTP, ProfileService: HealthHTTP, ProfileBot: HealthCompose, HealthProcess: HealthProcess, "": ""}
	for profile, want := range cases {
		testutil.Equal(t, (AppConf{Profile: profile}).HealthMode(), want)
	}
}

func TestDeployState(t *testing.T) {
	dir := t.TempDir()
	testutil.Equal(t, LoadState(dir), DeployState{})
	want := DeployState{Current: "sha-bbbbbbb", Previous: "sha-aaaaaaa"}
	testutil.NoErr(t, want.Save(dir))
	testutil.Equal(t, LoadState(dir), want)
}

func TestSha7(t *testing.T) {
	cases := map[string]string{"sha-0123456789": "0123456", "sha-abc": "abc", "abcdef0123": "abcdef0", "": "", plainTag: plainTag}
	for in, want := range cases {
		testutil.Equal(t, Sha7(in), want)
	}
}
