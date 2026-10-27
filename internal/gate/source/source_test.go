package source

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	docsState   = "docs/STATE.md"
	pkgA        = "package a\n"
	ignoreFile  = ".semgrepignore"
	fileMain    = "main.go"
	fileCI      = "ci.yml"
	fileReadme  = "README.md"
	fileBadYAML = "bad.yml"
	fileNested  = "a/b.go"
	fileA       = "a.go"
)

var (
	markerTodo  = strings.ToUpper("todo")
	markerFixme = strings.ToUpper("fixme")
)

func repo(t *testing.T, files map[string]string) gate.Workspace {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	ws := gate.Workspace{
		Src: dir, Out: t.TempDir(), Exec: procexec.NewOS(GitBinary), Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{Markers: []string{"todo", "fixme"}, ForbiddenFiles: []string{"*state*.md", "*.bak", ignoreFile}},
	}
	git(t, ws, "init", "-q")
	git(t, ws, "add", "-A")
	return ws
}

func git(t *testing.T, ws gate.Workspace, args ...string) {
	t.Helper()
	_, err := ws.Exec.Run(context.Background(), procexec.Command{Name: GitBinary, Args: append([]string{"-C", ws.Src}, args...)})
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, procexec.Output(err))
	}
}

func TestCommentsFoundInEveryLanguage(t *testing.T) {
	ws := repo(t, map[string]string{
		fileMain:               "package main\n\n// note\nfunc main() { /* inline */ }\n",
		"clean.go":             "package main\n\nvar s = \"// not a comment\"\n",
		"embed.go":             "//go:build linux\n\npackage main\n\nimport _ \"embed\"\n\n//go:embed static/*.css\nvar css string\n",
		fileCI:                 "on: push # trailing\njobs: {}\n",
		"compose.yaml":         "# head\nservices: {}\n",
		"Dockerfile":           "FROM scratch\n# comment\n",
		"build.Dockerfile":     "FROM scratch\n",
		"run.sh":               "#!/bin/sh\n  # indented\necho ok\n",
		"Makefile":             "all:\n\t# recipe comment\n",
		fileReadme:             "# Title\n",
		"blob.bin":             "\x00# not text\n",
		"blob.sh":              "\x00\n# binary with a NUL first\n",
		"late.sh":              "echo\n\x00# NUL later\n",
		"bad.go":               "package main\n\"unterminated\n",
		fileBadYAML:            "a: [\n",
		".gitignore":           "# ignored files\n*.out\n",
		"app.service":          "[Unit]\n#x\n",
		"nested/dir/script.py": "#!/usr/bin/env python3\nprint(1)\n",
	})
	out := Comments(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	want := []string{
		".gitignore:1", "Dockerfile:2", "Makefile:2", "app.service:2", "bad.go:2: string literal not terminated", fileBadYAML, "ci.yml:1",
		"compose.yaml:2", "main.go:3", "main.go:4", "run.sh:2",
	}
	testutil.Equal(t, len(out.Findings), len(want))
	for i, w := range want {
		testutil.Contains(t, out.Findings[i], w)
	}
	testutil.Equal(t, out.Values[gate.ValueFindings], float64(len(want)))
}

func TestCommentsInEveryStack(t *testing.T) {
	ws := repo(t, map[string]string{
		"web/app.js":           "const re = /\\/*x/;\nconst u = `a ${b} // no`;\nlet n = 1; // yes\n",
		"web/view.tsx":         "/* yes */\nexport const v = 1;\n",
		"web/page.html":        "<p>don't // text</p>\n<!-- yes -->\n",
		"web/site.css":         "a { background: url(http://x/y); }\n/* yes */\n",
		"svc/main.py":          "def f():\n    \"\"\"yes\"\"\"\n    return \"# no\"  # yes\n",
		"svc/Program.cs":       "var s = @\"C:\\x\\\"; // yes\n",
		"db/init.sql":          "SELECT 1; -- yes\n",
		"tsconfig.json":        "{\n  // yes\n  \"a\": \"//b\"\n}\n",
		"go.mod":               "module m\n\nrequire a v1.0.0 // indirect\nrequire b v1.0.0 // pinned\n",
		"go.sum":               "a v1.0.0 h1:x\n",
		"poetry.lock":          "# generated\n",
		"requirements-dev.txt": "# yes\npytest==9.0.0\n",
		"notes.txt":            "# prose\n",
		"docs/guide.md":        "```sh\necho # prose\n```\n",
		"settings.unknownext":  "# unknown format\n",
	})
	out := Comments(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{
		"db/init.sql:1", "go.mod:4", "requirements-dev.txt:1", "svc/Program.cs:1", "svc/main.py:2", "svc/main.py:3",
		"tsconfig.json:2", "web/app.js:3", "web/page.html:2", "web/site.css:2", "web/view.tsx:1",
	})
}

func TestCommentsReportLexerFailures(t *testing.T) {
	saved := lex
	t.Cleanup(func() { lex = saved })
	lex = func(string, []byte) ([]lexis.Token, bool, error) { return nil, true, errors.New("lexer broke") }
	ws := repo(t, map[string]string{"app.js": "let a = 1;\n"})
	testutil.Equal(t, Comments(context.Background(), ws).Findings, []string{"app.js: lexer broke"})
	testutil.Equal(t, noteLines("x.js", []lexis.Token{{Line: 2, Note: true}, {Line: 2, Note: true}, {Line: 3}, {Line: 4, Note: true}}), []string{"x.js:2", "x.js:4"})
}

func TestCleanRepositoryPasses(t *testing.T) {
	ws := repo(t, map[string]string{fileMain: "package main\n", fileCI: "on: push\n", fileReadme: "Notes\n"})
	testutil.Equal(t, Comments(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, Markers(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, Hygiene(context.Background(), ws).Status, gate.StatusPass)
}

func TestMarkers(t *testing.T) {
	ws := repo(t, map[string]string{
		fileA:      "package a\n\nvar x = \"" + markerTodo + " later\"\n",
		fileReadme: "Fix " + markerFixme + " soon\nthe todo list is lowercase\n" + markerTodo + "S are not markers\n",
	})
	out := Markers(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{fileReadme + ":1: " + markerFixme, "a.go:3: " + markerTodo})
}

func TestHygiene(t *testing.T) {
	ws := repo(t, map[string]string{docsState: "x", "main.go.bak": "x", ignoreFile: "", "ok.go": "package ok\n"})
	out := Hygiene(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{
		ignoreFile + " matches " + ignoreFile, docsState + " matches *state*.md", "main.go.bak matches *.bak",
	})
}

func TestHygieneHonoursAllowedFiles(t *testing.T) {
	ws := repo(t, map[string]string{docsState: "x", "docs/tasks/a/STATE.md": "x", "docs/x.bak": "x", "STATE.md": "x"})
	ws.App = policy.App{Allowed: []string{docsState, "docs/tasks/*/STATE.md"}}
	out := Hygiene(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{"STATE.md matches *state*.md", "docs/x.bak matches *.bak"})
}

func TestNotARepository(t *testing.T) {
	ws := gate.Workspace{Src: t.TempDir(), Exec: procexec.NewOS(GitBinary), Log: slog.New(slog.DiscardHandler)}
	for _, out := range []gate.Outcome{Comments(context.Background(), ws), Markers(context.Background(), ws), Hygiene(context.Background(), ws)} {
		testutil.Equal(t, out.Status, gate.StatusFail)
		testutil.Contains(t, out.Reason, "git ls-files")
	}
	testutil.Fails(t, CopyTracked(context.Background(), ws, t.TempDir()))
	ws.Src = filepath.Join(ws.Src, "missing")
	testutil.Fails(t, CopyTracked(context.Background(), ws, t.TempDir()))
}

func TestCopyTracked(t *testing.T) {
	ws := repo(t, map[string]string{fileNested: pkgA, "c.txt": "c"})
	testutil.NoErr(t, os.Symlink("c.txt", filepath.Join(ws.Src, "link")))
	git(t, ws, "add", "link")
	testutil.Write(t, filepath.Join(ws.Src, "untracked.txt"), "u")
	dest := filepath.Join(t.TempDir(), "copy")
	testutil.NoErr(t, CopyTracked(context.Background(), ws, dest))
	testutil.Equal(t, testutil.Read(t, filepath.Join(dest, "a", "b.go")), pkgA)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dest, "c.txt")), "c")
	testutil.Missing(t, filepath.Join(dest, "untracked.txt"))
	testutil.Missing(t, filepath.Join(dest, "link"))
	testutil.Perm(t, filepath.Join(dest, "c.txt"), copyFilePerm)
}

func TestSelectFiltersTrackedFiles(t *testing.T) {
	ws := repo(t, map[string]string{fileNested: pkgA, "notes.md": "c"})
	testutil.Write(t, filepath.Join(ws.Src, "untracked.go"), "package u\n")
	files, err := Select(context.Background(), ws, func(f string) bool { return filepath.Ext(f) == ".go" })
	testutil.NoErr(t, err)
	testutil.Equal(t, files, []string{fileNested})
	ws.Src = t.TempDir()
	_, err = Select(context.Background(), ws, func(string) bool { return true })
	testutil.Contains(t, err.Error(), "git ls-files")
}

func TestFixture(t *testing.T) {
	for _, rel := range []string{"testdata/a.js", "internal/store/testdata/old.sql", "a/testdata/b/c.go"} {
		testutil.True(t, Fixture(rel), rel)
	}
	for _, rel := range []string{"testdata.js", "internal/mytestdata/a.js", "a/testdata_x/b.go"} {
		testutil.True(t, !Fixture(rel), rel)
	}
}

func TestCopyTrackedFailures(t *testing.T) {
	ws := repo(t, map[string]string{fileNested: pkgA})
	blocker := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocker, "")
	testutil.Fails(t, CopyTracked(context.Background(), ws, filepath.Join(blocker, "dest")))
	dest := t.TempDir()
	testutil.Write(t, filepath.Join(dest, "a"), "a file where a directory is needed")
	testutil.Fails(t, CopyTracked(context.Background(), ws, dest))
	testutil.NoErr(t, os.Remove(filepath.Join(ws.Src, "a", "b.go")))
	testutil.NoErr(t, os.Mkdir(filepath.Join(ws.Src, "a", "b.go"), 0o700))
	testutil.NoErr(t, CopyTracked(context.Background(), ws, t.TempDir()))
	unreadable := repo(t, map[string]string{"secret.txt": "x"})
	testutil.NoErr(t, os.Chmod(filepath.Join(unreadable.Src, "secret.txt"), 0))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(unreadable.Src, "secret.txt"), 0o600) })
	testutil.Fails(t, CopyTracked(context.Background(), unreadable, t.TempDir()))
	gone := repo(t, map[string]string{"x.txt": "x"})
	testutil.NoErr(t, os.Remove(filepath.Join(gone.Src, "x.txt")))
	testutil.Fails(t, CopyTracked(context.Background(), gone, t.TempDir()))
}

func TestLanguage(t *testing.T) {
	ws := repo(t, map[string]string{
		fileA:      "package a\n\nvar s = \"café — ok\"\n",
		fileReadme: "English\n\u0441\u0442\u0440\u043e\u043a\u0430\n",
		"b.txt":    "plain\n\n\u65e5\u672c\n",
	})
	out := Language(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{fileReadme + ":2: '\u0441'", "b.txt:3: '\u65e5'"})
	clean := repo(t, map[string]string{fileA: "package a\n\nvar s = \"naïve façade 42 ✓\"\n"})
	testutil.Equal(t, Language(context.Background(), clean).Status, gate.StatusPass)
}

func TestLanguageSkipsLocalizedPaths(t *testing.T) {
	ws := repo(t, map[string]string{
		"internal/texts/ru.go": "package texts\n\nvar s = \"\u0441\u0442\u0440\u043e\u043a\u0430\"\n",
		"a_test.go":            "package a\n\nvar s = \"\u0441\"\n",
		fileReadme:             "\u0441\n",
	})
	ws.App = policy.App{Localized: []string{"internal/texts/", "*_test.go"}}
	out := Language(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{fileReadme + ":1: '\u0441'"})
}

func TestExtReadsTheShebangOfFilesWithoutCodeExtension(t *testing.T) {
	ws := repo(t, map[string]string{"bin/apply": "#!/bin/sh\necho hi\n", "bin/agent": "#!/usr/bin/ucode\n", "lib/x.go": pkgA, "etc/routerbot.init": "#!/bin/sh /etc/rc.common\n", "docs/a.md": "# A\n"})
	testutil.Equal(t, Ext(ws.Src, "etc/routerbot.init"), ".sh")
	testutil.Equal(t, Ext(ws.Src, "docs/a.md"), ".md")
	testutil.Equal(t, Ext(ws.Src, "docs/gone.md"), ".md")
	testutil.Equal(t, Ext(ws.Src, "bin/apply"), ".sh")
	testutil.Equal(t, Ext(ws.Src, "bin/agent"), "#!ucode")
	testutil.Equal(t, Ext(ws.Src, "lib/x.go"), ".go")
	testutil.Equal(t, Ext(ws.Src, "bin/missing"), "")
}

func TestCommentsInScriptsWithoutExtension(t *testing.T) {
	ws := repo(t, map[string]string{"agent/apply": "#!/bin/sh\n# yes\necho ok\n", "agent/init": "#!/bin/sh /etc/rc.common\nSTART=99\n"})
	testutil.Equal(t, Comments(context.Background(), ws).Findings, []string{"agent/apply:2"})
}
