package static

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/node"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	htmlTool  = "html-validate.mjs"
	styleTool = "stylelint.mjs"
	page      = "index.html"
	sheet     = "style.css"
	legacy    = "b.htm"
)

var errSim = errors.New("simulated")

func site(t *testing.T, handler func(procexec.Command) (procexec.Result, error), files ...string) gate.Workspace {
	t.Helper()
	ws := gate.Workspace{Src: t.TempDir(), Out: t.TempDir(), PolicyDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	testutil.Write(t, filepath.Join(ws.PolicyDir, "node", "tools.json"), "{}")
	testutil.Write(t, filepath.Join(ws.PolicyDir, "node", "tools-lock.json"), "{}")
	listing := strings.Join(files, "\x00") + "\x00"
	ws.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Name == "git" {
			return procexec.Result{Stdout: listing}, nil
		}
		if c.Name == node.NPMBinary {
			return procexec.Result{}, nil
		}
		return handler(c)
	}}
	return ws
}

func TestLintBothLinters(t *testing.T) {
	var ws gate.Workspace
	ws = site(t, func(c procexec.Command) (procexec.Result, error) {
		switch {
		case strings.HasSuffix(c.Args[0], htmlTool):
			return procexec.Result{Stdout: `[{"filePath":"` + ws.Src + `/index.html","source":"<html>","messages":[{"ruleId":"require-sri","message":"SRI required","line":3}]}]`}, errSim
		default:
			return procexec.Result{Stderr: `[{"source":"` + ws.Src + `/style.css","warnings":[{"rule":"color-named","text":"Unexpected named color","line":2}]}]`}, errSim
		}
	}, page, sheet, "app.js")
	out := Lint(context.Background(), ws)
	testutil.Equal(t, out.Findings, []string{"index.html:3 require-sri SRI required", "style.css:2 color-named Unexpected named color"})
	calls := procexectest.CallsOf(ws.Exec)
	html := calls[len(calls)-2]
	testutil.Equal(t, html.Args[1:], []string{configFlag, filepath.Join(ws.Report("node-tools"), node.HTMLValidateConfig), formatterFlag, formatterJSON + "=" + filepath.Join(ws.Report(templatesDir), htmlReport), page})
	style := calls[len(calls)-1]
	testutil.Equal(t, style.Args[1:], []string{configFlag, filepath.Join(ws.Report("node-tools"), node.StylelintConfig), "--ignore-disables", formatterFlag, formatterJSON, sheet})
}

func TestLintFailures(t *testing.T) {
	garbled := site(t, func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stdout: "oops"}, errSim }, page)
	testutil.Contains(t, Lint(context.Background(), garbled).Reason, htmlTool)
	onlyStyles := site(t, func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stderr: emptyJSON}, nil }, sheet)
	testutil.Equal(t, Lint(context.Background(), onlyStyles).Status, gate.StatusPass)
	unlisted := site(t, nil, page)
	unlisted.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	testutil.Contains(t, Lint(context.Background(), unlisted).Reason, "git ls-files")
	noTools := site(t, nil, page)
	noTools.PolicyDir = filepath.Join(t.TempDir(), "missing")
	testutil.Equal(t, Lint(context.Background(), noTools).Status, gate.StatusFail)
}

func TestNotApplicableChecks(t *testing.T) {
	for _, check := range []func(context.Context, gate.Workspace) gate.Outcome{NoCode, NoDeps} {
		out := check(context.Background(), gate.Workspace{})
		testutil.Equal(t, out.Status, gate.StatusNA)
	}
	testutil.Equal(t, NoCode(context.Background(), gate.Workspace{}).Reason, reasonNoCode)
	passed, err := Passed(context.Background(), gate.Workspace{})
	testutil.NoErr(t, err)
	testutil.Equal(t, len(passed), 0)
	testutil.Equal(t, filter([]string{page, legacy, sheet, "d.js"}, markup), []string{page, legacy})
}
