package gotmpl

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template/parse"
)

const (
	actionOpen   = "{{"
	actionClose  = "}}"
	errorPrefix  = "template: "
	RuleParse    = "go-template"
	RuleMissing  = "go-template-missing"
	missingFmt   = "no such template %q"
	lineBreak    = "\n"
	noLine       = 0
	firstLine    = 1
	firstVariant = 0
)

type Source struct {
	Path string
	Text string
}

type Finding struct {
	File    string
	Line    int
	Rule    string
	Message string
}

type Variant struct {
	HTML  string
	spans []span
}

type span struct {
	out, src, size int
	file           *Source
}

func IsTemplate(text string) bool {
	return strings.Contains(text, actionOpen)
}

func (v Variant) Origin(line, column int) (string, int) {
	off := offsetOf(v.HTML, line, column)
	s := v.spans[sort.Search(len(v.spans), func(i int) bool { return v.spans[i].out > off })-1]
	src := s.src + min(off-s.out, s.size)
	return s.file.Path, strings.Count(s.file.Text[:src], lineBreak) + firstLine
}

func offsetOf(text string, line, column int) int {
	off := 0
	for range line - firstLine {
		off += strings.Index(text[off:], lineBreak) + len(lineBreak)
	}
	return min(off+max(column-firstLine, 0), len(text))
}

type named struct {
	tree *parse.Tree
	src  *Source
}

func Expand(sources []Source) ([]Variant, []Finding) {
	trees, findings := parseAll(sources)
	if len(findings) > 0 {
		return nil, findings
	}
	called, findings := calls(trees)
	if len(findings) > 0 {
		return nil, findings
	}
	var variants []Variant
	seen := map[string]bool{}
	for _, name := range roots(trees, called) {
		for _, v := range expandRoot(trees, name) {
			if !seen[v.HTML] && strings.TrimSpace(v.HTML) != "" {
				seen[v.HTML] = true
				variants = append(variants, v)
			}
		}
	}
	return variants, nil
}

func parseAll(sources []Source) (map[string]named, []Finding) {
	trees := map[string]named{}
	var findings []Finding
	for i := range sources {
		src := &sources[i]
		t := parse.New(path.Base(src.Path))
		t.Mode = parse.SkipFuncCheck | parse.ParseComments
		set := map[string]*parse.Tree{}
		if _, err := t.Parse(src.Text, actionOpen, actionClose, set); err != nil {
			findings = append(findings, parseFinding(src.Path, t.ParseName, err))
			continue
		}
		for name, tree := range set {
			trees[name] = named{tree, src}
		}
	}
	return trees, findings
}

func parseFinding(file, name string, err error) Finding {
	msg := err.Error()
	line := noLine
	if rest, ok := strings.CutPrefix(msg, errorPrefix+name+":"); ok {
		num, text, _ := strings.Cut(rest, ": ")
		if n, aerr := strconv.Atoi(num); aerr == nil {
			line, msg = n, text
		}
	}
	return Finding{File: file, Line: line, Rule: RuleParse, Message: msg}
}

func calls(trees map[string]named) (map[string]bool, []Finding) {
	called := map[string]bool{}
	var findings []Finding
	for _, name := range sortedNames(trees) {
		src := trees[name].src
		visit(trees[name].tree.Root, func(n *parse.TemplateNode) {
			if n.Name != name {
				called[n.Name] = true
			}
			if _, ok := trees[n.Name]; !ok {
				findings = append(findings, Finding{File: src.Path, Line: lineAt(src.Text, n.Pos), Rule: RuleMissing, Message: fmt.Sprintf(missingFmt, n.Name)})
			}
		})
	}
	return called, findings
}

func visit(n parse.Node, found func(*parse.TemplateNode)) {
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			visit(c, found)
		}
	case *parse.TemplateNode:
		found(n)
	case *parse.IfNode:
		visitBranch(&n.BranchNode, found)
	case *parse.RangeNode:
		visitBranch(&n.BranchNode, found)
	case *parse.WithNode:
		visitBranch(&n.BranchNode, found)
	}
}

func visitBranch(b *parse.BranchNode, found func(*parse.TemplateNode)) {
	visit(b.List, found)
	if b.ElseList != nil {
		visit(b.ElseList, found)
	}
}

func roots(trees map[string]named, called map[string]bool) []string {
	return slices.DeleteFunc(sortedNames(trees), func(name string) bool { return called[name] })
}

func sortedNames(trees map[string]named) []string {
	names := make([]string, 0, len(trees))
	for name := range trees {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func lineAt(text string, pos parse.Pos) int {
	return strings.Count(text[:pos], lineBreak) + firstLine
}

func expandRoot(trees map[string]named, name string) []Variant {
	seen := map[choice]int{}
	var variants []Variant
	for k := firstVariant; ; k++ {
		e := &expander{trees: trees, seen: seen, stack: []string{name}}
		e.walk(trees[name].tree.Root, trees[name].src)
		if k > firstVariant && e.progress == 0 {
			return variants
		}
		variants = append(variants, Variant{HTML: e.out.String(), spans: e.spans})
	}
}
