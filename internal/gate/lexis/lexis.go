package lexis

import (
	"errors"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

const (
	docstringLanguage = "Python"
	directiveLanguage = "Go"
	tripleDouble      = `"""`
	tripleSingle      = "'''"
)

var codeExts = map[string]bool{
	".go": true, ".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".ts": true, ".mts": true, ".cts": true, ".tsx": true,
	".vue": true, ".svelte": true, ".py": true, ".pyi": true, ".rb": true, ".php": true, ".java": true, ".kt": true, ".kts": true,
	".scala": true, ".groovy": true, ".cs": true, ".fs": true, ".vb": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
	".cxx": true, ".hpp": true, ".rs": true, ".swift": true, ".dart": true, ".lua": true, ".pl": true, ".pm": true, ".sh": true,
	".bash": true, ".zsh": true, ".ps1": true, ".sql": true, ".r": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true,
	".clj": true, ".m": true, ".mm": true,
}

func IsCode(name string) bool {
	return codeExts[path.Ext(name)]
}

var (
	affixKind   = chroma.LiteralStringAffix.String()
	ErrEncoding = errors.New("source is not valid UTF-8")
)

type Token struct {
	Line   int
	Offset int
	Kind   string
	Text   string
	Note   bool
}

func Lex(name string, data []byte) ([]Token, bool, error) {
	l := lexers.Match(path.Base(name))
	if l == nil {
		return nil, false, nil
	}
	if !utf8.Valid(data) {
		return nil, true, ErrEncoding
	}
	it, err := chroma.Coalesce(l).Tokenise(nil, string(data))
	if err != nil {
		return nil, true, err
	}
	toks := significant(it.Tokens(), offsetMap(data))
	switch l.Config().Name {
	case docstringLanguage:
		markDocstrings(toks)
	case directiveLanguage:
		unmarkDirectives(toks)
	}
	return toks, true, nil
}

func significant(raw []chroma.Token, original func(int) int) []Token {
	out := make([]Token, 0, len(raw))
	line, offset := 1, 0
	for _, t := range raw {
		if !t.Type.InCategory(chroma.Text) {
			out = append(out, Token{Line: line, Offset: original(offset), Kind: t.Type.String(), Text: t.Value, Note: isComment(t.Type)})
		}
		line += strings.Count(t.Value, "\n")
		offset += len(t.Value)
	}
	return out
}

func offsetMap(data []byte) func(int) int {
	var dropped []int
	normalized := 0
	for i, b := range data {
		if b == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			dropped = append(dropped, normalized)
			continue
		}
		normalized++
	}
	return func(at int) int {
		return at + sort.SearchInts(dropped, at+1)
	}
}

var goDirectives = []string{"//go:", "//line ", "//export ", "//extern "}

func unmarkDirectives(toks []Token) {
	for i := range toks {
		if toks[i].Note && isDirective(toks[i].Text) {
			toks[i].Note = false
		}
	}
}

func isDirective(text string) bool {
	for _, prefix := range goDirectives {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func isComment(t chroma.TokenType) bool {
	return t.InCategory(chroma.Comment) && !t.InSubCategory(chroma.CommentPreproc) && t != chroma.CommentHashbang
}

type docScan struct {
	candidate *Token
	prevEnd   int
}

func (d *docScan) visit(t *Token) {
	if d.candidate != nil && t.Line > lastLine(*d.candidate) {
		d.candidate.Note = true
	}
	if t.Kind == affixKind {
		return
	}
	d.candidate = nil
	if isTripleQuoted(*t) && t.Line > d.prevEnd {
		d.candidate = t
	}
	d.prevEnd = lastLine(*t)
}

func markDocstrings(toks []Token) {
	var d docScan
	for i := range toks {
		d.visit(&toks[i])
	}
	if d.candidate != nil {
		d.candidate.Note = true
	}
}

func lastLine(t Token) int {
	return t.Line + strings.Count(t.Text, "\n")
}

func isTripleQuoted(t Token) bool {
	return strings.HasPrefix(t.Kind, chroma.LiteralString.String()) && (strings.HasPrefix(t.Text, tripleDouble) || strings.HasPrefix(t.Text, tripleSingle))
}
