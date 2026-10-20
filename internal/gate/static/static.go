package static

import (
	"context"
	"encoding/json"
	"fmt"
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
)

var (
	Sources = []string{extHTML, extHTM, extCSS}
	markup  = []string{extHTML, extHTM}
)

func NotApplicable(reason string) func(context.Context, gate.Workspace) gate.Outcome {
	return func(context.Context, gate.Workspace) gate.Outcome {
		return gate.Outcome{Status: gate.StatusNA, Reason: reason}
	}
}

var (
	NoCode = NotApplicable(reasonNoCode)
	NoDeps = NotApplicable(reasonNoDeps)
)

func Passed(context.Context, gate.Workspace) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	pages := filter(files, markup)
	styles := filter(files, []string{extCSS})
	dir, err := node.Tools(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	for _, run := range []struct {
		files []string
		args  []string
	}{
		{pages, []string{node.ToolScript(ws, "html-validate", "bin", "html-validate.mjs"), configFlag, filepath.Join(dir, node.HTMLValidateConfig), formatterFlag, formatterJSON}},
		{styles, []string{node.ToolScript(ws, "stylelint", "bin", "stylelint.mjs"), configFlag, filepath.Join(dir, node.StylelintConfig), "--ignore-disables", formatterFlag, formatterJSON}},
	} {
		found, lerr := lint(ctx, ws, run.files, run.args)
		if lerr != nil {
			return gate.Fail(lerr.Error())
		}
		findings = append(findings, found...)
	}
	return gate.Findings(reasonLint, findings)
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
	} `json:"messages"`
	Warnings []struct {
		Rule string `json:"rule"`
		Text string `json:"text"`
		Line int    `json:"line"`
	} `json:"warnings"`
}

func lint(ctx context.Context, ws gate.Workspace, files, args []string) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	res, err := ws.Run(ctx, node.NodeBinary, append(args, files...)...)
	output := res.Stdout
	if strings.TrimSpace(output) == "" {
		output = res.Stderr
	}
	var reports []report
	if perr := json.Unmarshal([]byte(output), &reports); perr != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(args[0]), perr, gate.Tail(procexec.Output(err)))
	}
	var out []string
	for _, r := range reports {
		out = append(out, r.findings(ws)...)
	}
	return out, nil
}

func (r report) findings(ws gate.Workspace) []string {
	file := r.FilePath
	if file == "" {
		file = r.Source
	}
	file = ws.Rel(file)
	out := make([]string, 0, len(r.Messages)+len(r.Warnings))
	for _, m := range r.Messages {
		out = append(out, fmt.Sprintf(findingForm, file, m.Line, m.RuleID, m.Message))
	}
	for _, w := range r.Warnings {
		out = append(out, fmt.Sprintf(findingForm, file, w.Line, w.Rule, w.Text))
	}
	return out
}
