package gotmpl

import (
	"errors"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	pageFile  = "web/templates/page.html"
	iconsFile = "web/templates/icons.html"
	paraA     = "<p>a</p>"
	paraEmpty = "<p></p>"
	paraX     = "<p>x</p>"
	listEmpty = "<ul></ul>"
	listTwo   = "<ul><li></li><li></li></ul>"
	paraC     = "<p>c</p>"
	paraY     = "<p>y</p>"
)

func pages(t *testing.T, sources ...Source) []string {
	t.Helper()
	variants, findings := Expand(sources)
	testutil.Equal(t, len(findings), 0)
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, v.HTML)
	}
	return out
}

func page(text string) Source {
	return Source{Path: pageFile, Text: text}
}

func TestConditionsCoverEveryBranch(t *testing.T) {
	testutil.Equal(t, pages(t, page(`{{define "page"}}<p>{{if .A}}a{{else if .B}}b{{else}}c{{end}}</p>{{end}}`)), []string{paraA, "<p>b</p>", paraC})
	testutil.Equal(t, pages(t, page(`<p>{{if .A}}a{{end}}</p>`)), []string{paraA, paraEmpty})
	testutil.Equal(t, pages(t, page(`<p>{{if .A}}{{if .B}}x{{else}}y{{end}}{{else}}z{{end}}</p>`)), []string{paraX, "<p>z</p>", paraY})
	testutil.Equal(t, pages(t, page(`<p>{{if .A}}a{{else}}{{if .B}}b{{end}}c{{end}}</p>`)), []string{paraA, "<p>bc</p>", paraC})
}

func TestRangeRepeatsItsBodyAndCoversElse(t *testing.T) {
	testutil.Equal(t, pages(t, page(`<ul>{{range .}}<li id="row">{{.}}</li>{{else}}<li>none</li>{{end}}</ul>`)), []string{`<ul><li id="row">1</li><li id="row">2</li></ul>`, `<ul><li>none</li></ul>`})
	testutil.Equal(t, pages(t, page(`<ul>{{range .}}<li>{{break}}</li>{{end}}</ul>`)), []string{listTwo, listEmpty})
}

func TestWithChainsCoverEveryBranch(t *testing.T) {
	testutil.Equal(t, pages(t, page(`<p>{{with .X}}<b>x</b>{{else with .Y}}<i>y</i>{{else}}-{{end}}</p>`)), []string{"<p><b>x</b></p>", "<p><i>y</i></p>", "<p>-</p>"})
	testutil.Equal(t, pages(t, page(`<p>{{with .X}}x{{else}}{{if .Y}}y{{end}}{{end}}</p>`)), []string{paraX, paraY, paraEmpty})
}

func TestNestedTemplatesRenderInTheCallersContext(t *testing.T) {
	icons := Source{Path: iconsFile, Text: "{{define `icon`}}<svg>{{template `glyph` .}}</svg>{{end}}\n{{define `glyph`}}\n{{- if eq . `dot`}}<circle r=\"1\"/>{{else}}<path d=\"M0\"/>{{end}}{{end}}"}
	got := pages(t, icons, page(`{{define "page"}}<button type="button">{{template "icon" "dot"}}</button>{{end}}`))
	testutil.Equal(t, got, []string{`<button type="button"><svg><circle r="1"/></svg></button>`, `<button type="button"><svg><path d="M0"/></svg></button>`})
}

func TestRecursiveTemplatesStop(t *testing.T) {
	testutil.Equal(t, pages(t, page(`{{define "tree"}}<ul>{{range .}}<li>{{template "tree" .}}</li>{{end}}</ul>{{end}}`)), []string{listTwo, listEmpty})
}

func TestActionsBecomeValuesOnlyWhereMarkupTakesThem(t *testing.T) {
	got := pages(t, page(`<a href="{{.U}}" {{.Attrs}} title='{{.T}}' data-n={{.N}}>{{$v := .}}{{.Text}}</a><!-- {{.C}} --><p>a < {{.B}}</p><{{.Tag}}>`))
	testutil.Equal(t, got, []string{`<a href="1"  title='2' data-n=3>4</a><!--  --><p>a < 5</p><6>`})
}

func TestOriginPointsAtTheTemplateSource(t *testing.T) {
	icons := Source{Path: iconsFile, Text: "{{define `icon`}}<svg>\n{{template `glyph`}}</svg>{{end}}\n{{define `glyph`}}\n\n<path d=\"{{.D}}\"/>{{end}}"}
	variants, _ := Expand([]Source{icons, page("{{define `page`}}<div>\n{{template `icon`}}\n</div>{{end}}")})
	testutil.Equal(t, variants[0].HTML, "<div>\n<svg>\n\n\n<path d=\"1\"/></svg>\n</div>")
	at, line := variants[0].Origin(5, 1)
	testutil.Equal(t, at, iconsFile)
	testutil.Equal(t, line, 5)
	at, line = variants[0].Origin(5, 10)
	testutil.Equal(t, []any{at, line}, []any{iconsFile, 5})
	at, line = variants[0].Origin(2, 0)
	testutil.Equal(t, []any{at, line}, []any{iconsFile, 1})
	at, line = variants[0].Origin(6, 1)
	testutil.Equal(t, []any{at, line}, []any{pageFile, 3})
	at, line = variants[0].Origin(9, 1)
	testutil.Equal(t, []any{at, line}, []any{pageFile, 3})
}

func TestBrokenTemplatesAreFindings(t *testing.T) {
	variants, findings := Expand([]Source{page("<p>\n{{if .A}}a</p>"), {Path: iconsFile, Text: "<i>{{.}}</i>"}})
	testutil.Equal(t, len(variants), 0)
	testutil.Equal(t, findings, []Finding{{File: pageFile, Line: 2, Rule: RuleParse, Message: "unexpected EOF"}})
	variants, findings = Expand([]Source{page("<p>\n{{template `nope`}}</p>")})
	testutil.Equal(t, len(variants), 0)
	testutil.Equal(t, findings, []Finding{{File: pageFile, Line: 2, Rule: RuleMissing, Message: `no such template "nope"`}})
}

func TestParseFindingKeepsUnusualErrors(t *testing.T) {
	f := parseFinding(pageFile, "page.html", errors.New("template: page.html:x: odd"))
	testutil.Equal(t, f, Finding{File: pageFile, Line: 0, Rule: RuleParse, Message: "template: page.html:x: odd"})
	f = parseFinding(pageFile, "page.html", errors.New("other"))
	testutil.Equal(t, f.Message, "other")
}

func TestIsTemplate(t *testing.T) {
	testutil.True(t, IsTemplate("<p>{{.}}</p>"), "action")
	testutil.True(t, !IsTemplate("<p>{ x }</p>"), "plain")
}
