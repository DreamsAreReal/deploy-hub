package static

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	module    = "go.mod"
	template  = "web/templates/page.html"
	icons     = "web/templates/icons.html"
	emptyJSON = "[]"
	pageText  = "{{define `page`}}<main>\n{{if .A}}<h1>{{.T}}</h1>{{else}}<h1></h1>{{end}}\n</main>{{end}}"
)

type validated struct {
	files []string
	saved string
}

func templateSite(t *testing.T, contents map[string]string, report func(validated) string, files ...string) (gate.Workspace, *validated) {
	t.Helper()
	seen := &validated{}
	ws := site(t, func(c procexec.Command) (procexec.Result, error) {
		if !strings.HasSuffix(c.Args[0], htmlTool) {
			return procexec.Result{Stderr: emptyJSON}, nil
		}
		seen.saved = strings.TrimPrefix(c.Args[4], formatterJSON+"=")
		seen.files = c.Args[5:]
		testutil.Write(t, seen.saved, report(*seen))
		return procexec.Result{}, nil
	}, files...)
	for name, text := range contents {
		testutil.Write(t, filepath.Join(ws.Src, name), text)
	}
	return ws, seen
}

func noFindings(validated) string { return emptyJSON }

func TestLintValidatesExpandedTemplates(t *testing.T) {
	ws, seen := templateSite(t, map[string]string{template: pageText, page: "<p>{ plain }</p>"}, func(v validated) string {
		var out []string
		for _, f := range v.files {
			if strings.HasPrefix(f, string(filepath.Separator)) {
				out = append(out, `{"filePath":"`+f+`","messages":[{"ruleId":"empty-heading","message":"empty","line":2,"column":1}]}`)
			}
		}
		return "[" + strings.Join(out, ",") + "]"
	}, module, template, page, sheet)
	out := Lint(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{"web/templates/page.html:2 empty-heading empty"})
	testutil.Equal(t, seen.files[0], page)
	testutil.Equal(t, len(seen.files), 3)
	testutil.Equal(t, testutil.Read(t, seen.files[1]), "<main>\n<h1>1</h1>\n</main>")
	testutil.Equal(t, testutil.Read(t, seen.files[2]), "<main>\n<h1></h1>\n</main>")
	testutil.True(t, !slices.Contains(seen.files, template), "the raw template is not validated")
}

func TestLintPassesCleanTemplates(t *testing.T) {
	ws, seen := templateSite(t, map[string]string{"app/" + template: pageText}, noFindings, "app/"+module, "app/"+template)
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, len(seen.files), 2)
}

func TestLintKeepsMarkupOutsideGoModulesAsIs(t *testing.T) {
	ws, seen := templateSite(t, map[string]string{template: pageText}, noFindings, "tool/"+module, template)
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, seen.files, []string{template})
	ws, seen = templateSite(t, map[string]string{template: "<main></main>"}, noFindings, module, template)
	Lint(context.Background(), ws)
	testutil.Equal(t, seen.files, []string{template})
}

func TestLintFailsOnTemplatesItCannotExpand(t *testing.T) {
	contents := map[string]string{template: "<main>\n{{if .A}}</main>", icons: "{{define `icon`}}<svg></svg>{{end}}", page: "<p></p>"}
	ws, seen := templateSite(t, contents, noFindings, module, template, icons, page)
	out := Lint(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{"web/templates/page.html:2 go-template unexpected EOF"})
	testutil.Equal(t, seen.files, []string{page})
	ws, _ = templateSite(t, map[string]string{template: "{{template `gone`}}"}, noFindings, module, template)
	testutil.Equal(t, Lint(context.Background(), ws).Findings, []string{`web/templates/page.html:1 go-template-missing no such template "gone"`})
}

func TestLintFailsWhenTemplatesCannotBeRead(t *testing.T) {
	ws, _ := templateSite(t, nil, noFindings, module, template)
	testutil.Contains(t, Lint(context.Background(), ws).Reason, "page.html")
	ws, _ = templateSite(t, map[string]string{template: pageText}, noFindings, module, template)
	ws.Out = filepath.Join(t.TempDir(), "out")
	testutil.Write(t, ws.Out, "a file where reports go")
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusFail)
}
