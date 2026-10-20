package dupl

import (
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	ValueDuplication = "duplication_percent"
	ValueCodeLines   = "code_lines"
	ValueDupLines    = "duplicated_lines"
	extGo            = ".go"
	percentScale     = 100
	reasonDupl       = "duplicated code is not below the policy limit"
	reasonUnreadable = "source files could not be tokenized"
	preprocKind      = "CommentPreproc"
	rangeFormat      = "%d-%d"
	fileFormat       = "%s: lines %s"
	hashBase         = 1099511628211
)

type tok struct {
	id   uint64
	line int
}

type unit struct {
	name   string
	tokens []tok
}

type lineKey struct {
	file string
	line int
}

type Report struct {
	CodeLines       int
	DuplicatedLines int
	Duplicated      map[string][]int
	Unreadable      []string
}

var lex = lexis.Lex

var headerWords = map[string]bool{"import": true, "from": true, "using": true, "use": true, "package": true, "require": true}

func (r Report) Percent() float64 {
	if r.CodeLines == 0 {
		return 0
	}
	return float64(r.DuplicatedLines) * percentScale / float64(r.CodeLines)
}

func Check(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	sources := map[string][]byte{}
	for _, f := range files {
		if !lexis.IsCode(f) || source.Fixture(f) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ws.Src, filepath.FromSlash(f)))
		if err != nil {
			return gate.Fail(err.Error())
		}
		sources[f] = data
	}
	rep := Analyze(sources, ws.Policy.Thresholds.DuplicateTokens)
	if len(rep.Unreadable) > 0 {
		return gate.Fail(reasonUnreadable, rep.Unreadable...)
	}
	pct := rep.Percent()
	values := map[string]float64{ValueDuplication: pct, ValueCodeLines: float64(rep.CodeLines), ValueDupLines: float64(rep.DuplicatedLines)}
	return gate.Judge(pct < ws.Policy.Thresholds.MaxDuplication, reasonDupl, values, rep.Findings())
}

func Analyze(sources map[string][]byte, window int) Report {
	interner := map[string]uint64{}
	units := make([]unit, 0, len(sources))
	code := map[lineKey]bool{}
	var unreadable []string
	for n, data := range sources {
		u, err := tokenize(n, data, interner)
		if err != nil {
			unreadable = append(unreadable, n+": "+err.Error())
		}
		for _, t := range u.tokens {
			code[lineKey{n, t.line}] = true
		}
		units = append(units, u)
	}
	dup := duplicatedLines(units, window)
	sort.Strings(unreadable)
	return Report{CodeLines: len(code), DuplicatedLines: len(dup), Duplicated: byFile(dup), Unreadable: unreadable}
}

func tokenize(name string, data []byte, interner map[string]uint64) (unit, error) {
	if path.Ext(name) == extGo {
		return goTokens(name, data, interner), nil
	}
	toks, _, err := lex(name, data)
	u := unit{name: name}
	line, header := 0, false
	for _, t := range toks {
		if t.Note {
			continue
		}
		if t.Line != line {
			line, header = t.Line, headerWords[t.Text] || strings.HasPrefix(t.Kind, preprocKind)
		}
		if !header {
			u.tokens = append(u.tokens, tok{id: intern(interner, t.Kind+" "+t.Text), line: t.Line})
		}
	}
	return u, err
}

func intern(interner map[string]uint64, key string) uint64 {
	id, ok := interner[key]
	if !ok {
		id = uint64(len(interner))
		interner[key] = id
	}
	return id
}

func goTokens(name string, data []byte, interner map[string]uint64) unit {
	fset := token.NewFileSet()
	file := fset.AddFile(name, fset.Base(), len(data))
	var s scanner.Scanner
	s.Init(file, data, nil, 0)
	u := unit{name: name}
	skip := skipper{}
	for {
		pos, kind, lit := s.Scan()
		if kind == token.EOF {
			return u
		}
		if skip.header(kind) {
			continue
		}
		u.tokens = append(u.tokens, tok{id: intern(interner, kind.String()+" "+lit), line: fset.Position(pos).Line})
	}
}

type skipper struct {
	inHeader bool
	depth    int
}

func (s *skipper) header(kind token.Token) bool {
	switch {
	case kind == token.PACKAGE || kind == token.IMPORT:
		s.inHeader = true
		return true
	case !s.inHeader:
		return false
	case kind == token.LPAREN:
		s.depth++
	case kind == token.RPAREN:
		s.depth--
	case kind == token.SEMICOLON && s.depth == 0:
		s.inHeader = false
	}
	return true
}

type occurrence struct {
	unit  int
	start int
}

func duplicatedLines(units []unit, window int) map[lineKey]bool {
	groups := map[uint64][]occurrence{}
	for ui, u := range units {
		for start, h := range windowHashes(u.tokens, window) {
			groups[h] = append(groups[h], occurrence{unit: ui, start: start})
		}
	}
	dup := map[lineKey]bool{}
	for _, occ := range groups {
		for i, a := range occ {
			if matchesAnother(units, occ, i, window) {
				markLines(dup, units[a.unit], a.start, window)
			}
		}
	}
	return dup
}

func windowHashes(tokens []tok, window int) []uint64 {
	if len(tokens) < window {
		return nil
	}
	var power uint64 = 1
	for range window - 1 {
		power *= hashBase
	}
	out := make([]uint64, 0, len(tokens))
	var h uint64
	for i, t := range tokens {
		if i >= window {
			h -= tokens[i-window].id * power
		}
		h = h*hashBase + t.id
		if i >= window-1 {
			out = append(out, h)
		}
	}
	return out
}

func matchesAnother(units []unit, occ []occurrence, i, window int) bool {
	a := occ[i]
	for j, b := range occ {
		if j != i && equalWindow(units[a.unit].tokens[a.start:a.start+window], units[b.unit].tokens[b.start:b.start+window]) {
			return true
		}
	}
	return false
}

func equalWindow(a, b []tok) bool {
	for k := range a {
		if a[k].id != b[k].id {
			return false
		}
	}
	return true
}

func markLines(dup map[lineKey]bool, u unit, start, window int) {
	for _, t := range u.tokens[start : start+window] {
		dup[lineKey{u.name, t.line}] = true
	}
}

func byFile(dup map[lineKey]bool) map[string][]int {
	out := map[string][]int{}
	for k := range dup {
		out[k.file] = append(out[k.file], k.line)
	}
	for f := range out {
		sort.Ints(out[f])
	}
	return out
}

func (r Report) Findings() []string {
	files := make([]string, 0, len(r.Duplicated))
	for f := range r.Duplicated {
		files = append(files, f)
	}
	sort.Strings(files)
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, fmt.Sprintf(fileFormat, f, ranges(r.Duplicated[f])))
	}
	return out
}

func ranges(lines []int) string {
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		parts = append(parts, fmt.Sprintf(rangeFormat, lines[i], lines[j]))
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
