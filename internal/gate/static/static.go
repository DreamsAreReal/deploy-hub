package static

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/node"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Stack         = "static"
	extCSS        = ".css"
	extHTML       = ".html"
	extHTM        = ".htm"
	findingForm   = "%s:%d %s %s"
	reasonLint    = "markup or stylesheets have findings"
	reasonNoCode  = "static markup and styles contain no executable code"
	reasonNoDeps  = "a static site has no package dependencies; external resources must carry SRI hashes (lint)"
	formatterFlag = "--formatter"
	formatterJSON = "json"
	configFlag    = "--config"
	htmlReport    = "html-validate.json"
)

type linter struct {
	files []string
	args  []string
	saved string
}

var (
	Sources = []string{extHTML, extHTM, extCSS}
	markup  = []string{extHTML, extHTM}
)

var (
	NoCode = gate.NotApplicable(reasonNoCode)
	NoDeps = gate.NotApplicable(reasonNoDeps)
)

func Passed(context.Context, gate.Workspace) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	x, err := expand(ws, files, filter(files, markup))
	if err != nil {
		return gate.Fail(err.Error())
	}
	styles := filter(files, []string{extCSS})
	dir, err := node.Tools(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	findings := x.findings
	saved := filepath.Join(ws.Report(templatesDir), htmlReport)
	for _, run := range []linter{
		{x.pages, []string{node.ToolScript(ws, "html-validate", "bin", "html-validate.mjs"), configFlag, filepath.Join(dir, node.HTMLValidateConfig), formatterFlag, formatterJSON + "=" + saved}, saved},
		{styles, []string{node.ToolScript(ws, "stylelint", "bin", "stylelint.mjs"), configFlag, filepath.Join(dir, node.StylelintConfig), "--ignore-disables", formatterFlag, formatterJSON}, ""},
	} {
		found, lerr := lint(ctx, ws, run, &x)
		if lerr != nil {
			return gate.Fail(lerr.Error())
		}
		findings = append(findings, found...)
	}
	return gate.Findings(reasonLint, unique(findings))
}

func unique(findings []string) []string {
	seen := map[string]bool{}
	return slices.DeleteFunc(findings, func(f string) bool {
		dup := seen[f]
		seen[f] = true
		return dup
	})
}

func filter(files, exts []string) []string {
	var out []string
	for _, f := range files {
		if slices.Contains(exts, path.Ext(f)) {
			out = append(out, f)
		}
	}
	return out
}

type report struct {
	FilePath string `json:"filePath"`
	Source   string `json:"source"`
	Messages []struct {
		RuleID  string `json:"ruleId"`
		Message string `json:"message"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
	} `json:"messages"`
	Warnings []struct {
		Rule string `json:"rule"`
		Text string `json:"text"`
		Line int    `json:"line"`
	} `json:"warnings"`
}

func lint(ctx context.Context, ws gate.Workspace, run linter, x *expansion) ([]string, error) {
	if len(run.files) == 0 {
		return nil, nil
	}
	res, err := ws.Run(ctx, node.NodeBinary, append(run.args, run.files...)...)
	output := toolOutput(res, run.saved)
	var reports []report
	if perr := json.Unmarshal([]byte(output), &reports); perr != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(run.args[0]), perr, gate.Tail(procexec.Output(err)))
	}
	var out []string
	for _, r := range reports {
		out = append(out, r.findings(ws, x)...)
	}
	return out, nil
}

func (r report) findings(ws gate.Workspace, x *expansion) []string {
	file := r.FilePath
	if file == "" {
		file = r.Source
	}
	out := make([]string, 0, len(r.Messages)+len(r.Warnings))
	for _, m := range r.Messages {
		at, line := x.locate(ws, file, m.Line, m.Column)
		out = append(out, fmt.Sprintf(findingForm, at, line, m.RuleID, m.Message))
	}
	for _, w := range r.Warnings {
		out = append(out, fmt.Sprintf(findingForm, ws.Rel(file), w.Line, w.Rule, w.Text))
	}
	return out
}

func toolOutput(res procexec.Result, saved string) string {
	if data, err := os.ReadFile(filepath.Clean(saved)); err == nil {
		return string(data)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return res.Stderr
	}
	return res.Stdout
}
