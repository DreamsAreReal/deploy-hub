package scaffold_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/onboard"
	"github.com/DreamsAreReal/deploy-hub/internal/scaffold"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	owner     = "DreamsAreReal"
	app       = "notes"
	hubRepo   = "DreamsAreReal/deploy-hub"
	port      = "9031"
	template  = "../../templates/service"
	goMod     = "go.mod"
	stubFile  = ".github/workflows/deploy.yml"
	hubMarker = "\nhub:\n"
	portFlag  = "--port"
)

var errBroken = errors.New("broken disk")

func options(t *testing.T, args ...string) scaffold.Options {
	t.Helper()
	opts, err := scaffold.ParseOptions(append([]string{app, portFlag, port}, args...), owner)
	testutil.NoErr(t, err)
	return opts
}

func render(t *testing.T, opts scaffold.Options) map[string][]byte {
	t.Helper()
	files, err := scaffold.Render(os.DirFS(template), opts, hubRepo)
	testutil.NoErr(t, err)
	return files
}

func TestOptionsDefaultToTheOwnerRepoAndADirectoryNamedLikeTheApp(t *testing.T) {
	opts := options(t)
	testutil.Equal(t, opts, scaffold.Options{App: app, Repo: owner + "/" + app, Port: port, Dir: app, Template: scaffold.DefaultTemplate})
	testutil.Equal(t, opts.Owner(), owner)
	custom := options(t, "--repo", "someone/notes-api", "--dir", "/tmp/x", "--template", "t")
	testutil.Equal(t, custom, scaffold.Options{App: app, Repo: "someone/notes-api", Port: port, Dir: "/tmp/x", Template: "t"})
	testutil.Equal(t, custom.Owner(), "someone")
}

func TestOptionsRefuseWhatTheHubWouldRefuse(t *testing.T) {
	for name, args := range map[string][]string{
		"nothing":     nil,
		"flag first":  {portFlag, port, app},
		"no port":     {app},
		"extra word":  {app, portFlag, port, "more"},
		"unknown":     {app, portFlag, port, "--profile", "bot"},
		"bad app":     {"Notes!", portFlag, port},
		"bad port":    {app, portFlag, "http"},
		"bad repo":    {app, portFlag, port, "--repo", "no-slash"},
		"port too hi": {app, portFlag, "70000"},
	} {
		if _, err := scaffold.ParseOptions(args, owner); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
	_, err := scaffold.ParseOptions(nil, owner)
	testutil.Is(t, err, scaffold.ErrUsage)
}

func TestTemplateBecomesTheAppWithTheHubCaller(t *testing.T) {
	opts := options(t)
	files := render(t, opts)
	for name, body := range files {
		testutil.Excludes(t, name+"\n"+string(body), "starter", "9099")
	}
	testutil.True(t, strings.HasPrefix(string(files[goMod]), "module github.com/DreamsAreReal/notes\n"), string(files[goMod]))
	testutil.Contains(t, string(files["docker-compose.yml"]), `"127.0.0.1:9031:8080"`, "container_name: notes", `"/notes", "-healthcheck"`)
	testutil.Contains(t, string(files["Dockerfile"]), "./cmd/notes", `ENTRYPOINT ["/notes"]`)
	testutil.Contains(t, string(files["internal/app/app.go"]), `"github.com/DreamsAreReal/notes/internal/web"`)
	testutil.True(t, files["cmd/notes/main.go"] != nil, "no main package")
	stub := onboard.Options{App: app, Dockerfile: "Dockerfile"}
	testutil.Equal(t, onboard.StubPath(stub), stubFile)
	testutil.Equal(t, string(files[stubFile]), onboard.Stub("main", hubRepo, stub))
}

func TestRepoNamedLikeThePlaceholderStaysIntact(t *testing.T) {
	files := render(t, options(t, "--repo", "starter-lab/starter-notes"))
	testutil.True(t, strings.HasPrefix(string(files[goMod]), "module github.com/starter-lab/starter-notes\n"), string(files[goMod]))
}

func TestEntriesPassTheHubsOwnParsersAndMatchTheRepo(t *testing.T) {
	opts := options(t)
	gate := string(embedded.Gate())
	at := strings.Index(gate, hubMarker)
	testutil.True(t, at > 0, "no hub section in gate.yaml")
	parsed, err := policy.Parse([]byte(gate[:at+1] + scaffold.GateEntry(opts) + gate[at+1:]))
	testutil.NoErr(t, err)
	entry := parsed.Apps[app]
	testutil.Equal(t, entry.Repo, opts.Repo)
	testutil.Equal(t, entry.Port, 8080)
	testutil.Equal(t, len(entry.Scenarios), 2)
	catalog, err := arch.Parse(append(bytes.Clone(embedded.Architecture()), scaffold.CatalogEntry(opts)...))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(catalog.Problems(app, time.Now())), 0)
	dir := filepath.Join(t.TempDir(), app)
	testutil.NoErr(t, scaffold.Write(dir, render(t, opts)))
	facts, err := arch.Gather(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, catalog.Merged([]string{app}).Drift(facts), []string(nil))
	testutil.Contains(t, scaffold.TargetFile(opts), `- targets: ["notes:9100"]`, "labels: {app: notes}")
	testutil.Contains(t, scaffold.NextSteps(opts), "deploy-hub onboard DreamsAreReal/notes", "targets/notes.yml", scaffold.GateEntry(opts), scaffold.CatalogEntry(opts))
}

func TestWriteFillsANewDirectoryOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), app)
	files := map[string][]byte{"b/c.txt": []byte("c"), "a.txt": []byte("a")}
	testutil.NoErr(t, scaffold.Write(dir, files))
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, "b", "c.txt")), "c")
	testutil.Is(t, scaffold.Write(dir, files), scaffold.ErrDirInUse)
	empty := t.TempDir()
	testutil.NoErr(t, scaffold.Write(empty, files))
	blocked := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocked, "x")
	testutil.Fails(t, scaffold.Write(blocked, files))
	testutil.Fails(t, scaffold.Write(t.TempDir(), map[string][]byte{"bad\x00name": nil}))
}

type brokenFS struct{ fstest.MapFS }

func (b brokenFS) ReadFile(name string) ([]byte, error) {
	if name == goMod {
		return nil, errBroken
	}
	return b.MapFS.ReadFile(name)
}

func TestMissingOrBrokenTemplateIsAnError(t *testing.T) {
	opts := options(t)
	_, err := scaffold.Render(os.DirFS(filepath.Join(t.TempDir(), "none")), opts, hubRepo)
	testutil.Is(t, err, scaffold.ErrNoTemplate)
	_, err = scaffold.Render(fstest.MapFS{"README.md": {Data: []byte("x")}}, opts, hubRepo)
	testutil.Is(t, err, scaffold.ErrNoTemplate)
	_, err = scaffold.Render(brokenFS{fstest.MapFS{goMod: {Data: []byte("module x")}}}, opts, hubRepo)
	testutil.Is(t, err, errBroken)
}
