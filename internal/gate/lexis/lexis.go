package lexis

import (
	"path"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

const (
	docstringLanguage = "Python"
	tripleDouble      = `"""`
	tripleSingle      = "'''"
)

var affixKind = chroma.LiteralStringAffix.String()

type Token struct {
	Line int
	Kind string
	Text string
	Note bool
}

func Lex(name string, data []byte) ([]Token, bool, error) {
	l := lexers.Match(path.Base(name))
	if l == nil {
		return nil, false, nil
	}
	it, err := chroma.Coalesce(l).Tokenise(nil, string(data))
	if err != nil {
		return nil, true, err
	}
	toks := significant(it.Tokens())
	if l.Config().Name == docstringLanguage {
		markDocstrings(toks)
	}
	return toks, true, nil
}

func significant(raw []chroma.Token) []Token {
	out := make([]Token, 0, len(raw))
	line := 1
	for _, t := range raw {
		if !t.Type.InCategory(chroma.Text) {
			out = append(out, Token{Line: line, Kind: t.Type.String(), Text: t.Value, Note: isComment(t.Type)})
		}
		line += strings.Count(t.Value, "\n")
	}
	return out
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
