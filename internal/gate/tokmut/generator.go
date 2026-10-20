package tokmut

import (
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
)

const (
	wordNot   = "not"
	wordIs    = "is"
	wordIn    = "in"
	wordFor   = "for"
	wordIf    = "if"
	wordAs    = "as"
	opColon   = ":"
	opMinus   = "-"
	opAssign  = "="
	openBody  = "{"
	closeBody = "}"
	space     = " "
	emptySet  = "()"
)

type generator struct {
	fam  Family
	file string
	src  []byte
	toks []token
	out  []mutation.Mutant
}

func (g *generator) run() {
	for i := range g.toks {
		g.swapAt(i)
		g.membershipAt(i)
		g.negatorAt(i)
		g.returnAt(i)
		g.conditionAt(i)
		g.exceptAt(i)
	}
	g.statements()
}

func (g *generator) add(op string, from, to int, edits ...mutation.Edit) {
	line := g.toks[from].line
	start, end := g.toks[from].offset, g.end(to)
	g.out = append(g.out, mutation.NewMutant(g.file, line, op, string(g.src[start:end]), start, edits...))
}

func (g *generator) end(i int) int {
	return g.toks[i].offset + len(g.toks[i].text)
}

func (g *generator) at(i int) token {
	if i < 0 || i >= len(g.toks) {
		return token{kind: kindNewline}
	}
	return g.toks[i]
}

func (g *generator) adjacent(i int) bool {
	return g.at(i+1).offset == g.end(i) && g.at(i+1).kind == kindWord
}

func (g *generator) swapAt(i int) {
	t := g.toks[i]
	text, last := t.text, i
	if fused := opMinus + g.at(i+1).text; t.text == opMinus && g.adjacent(i) && g.fam.Swaps[fused] != nil {
		text, last = fused, i+1
	}
	if t.kind != kindOp && t.kind != kindWord || !g.swappable(i, text) {
		return
	}
	for _, s := range g.fam.Swaps[text] {
		g.add(s.op, i, last, mutation.Edit{Offset: t.offset, Length: len(text), Text: s.to})
	}
}

func (g *generator) swappable(i int, text string) bool {
	if text != opAssign {
		return true
	}
	at := g.toks[i].offset
	return g.fam.Name == Shell.Name && at > 0 && g.src[at-1] == ' ' && at+1 < len(g.src) && g.src[at+1] == ' '
}

func (g *generator) membershipAt(i int) {
	if g.fam.Name != Python.Name || g.toks[i].kind != kindWord {
		return
	}
	t, next := g.toks[i], g.at(i+1)
	switch {
	case t.text == wordIs && next.text == wordNot:
		g.add(mutation.OpComparison, i, i+1, mutation.Edit{Offset: t.offset, Length: g.end(i+1) - t.offset, Text: wordIs})
	case t.text == wordIs:
		g.add(mutation.OpComparison, i, i, mutation.Edit{Offset: g.end(i), Text: space + wordNot})
	case t.text == wordNot && next.text == wordIn:
		g.add(mutation.OpComparison, i, i+1, mutation.Edit{Offset: t.offset, Length: g.end(i+1) - t.offset, Text: wordIn})
	case t.text == wordIn && g.at(i-1).text != wordNot && !g.inLoopHeader(i):
		g.add(mutation.OpComparison, i, i, mutation.Edit{Offset: t.offset, Text: wordNot + space})
	}
}

func (g *generator) inLoopHeader(i int) bool {
	for j := i - 1; j >= 0 && g.toks[j].kind != kindNewline; j-- {
		switch g.toks[j].text {
		case wordFor:
			return true
		case wordIf:
			return false
		}
	}
	return false
}

func (g *generator) negatorAt(i int) {
	t := g.toks[i]
	if !g.fam.Negators[t.text] || g.at(i-1).text == wordIs || g.at(i+1).text == wordIn {
		return
	}
	next := g.at(i + 1)
	if next.kind == kindNewline || next.kind == kindEnd {
		return
	}
	g.add(mutation.OpRemoveNegation, i, i+1, mutation.Edit{Offset: t.offset, Length: next.offset - t.offset})
}

func (g *generator) returnAt(i int) {
	t := g.toks[i]
	if t.kind != kindWord || t.text != g.fam.ReturnWord {
		return
	}
	last := g.expressionEnd(i + 1)
	if last <= i {
		return
	}
	value := string(g.src[g.toks[i+1].offset:g.end(last)])
	repl := g.fam.Null
	if value == repl {
		repl = alternativeNull
	}
	if g.fam.Name != Shell.Name && repl == alternativeNull {
		return
	}
	g.add(mutation.OpReturnValue, i, last, mutation.Edit{Offset: g.toks[i+1].offset, Length: len(value), Text: repl})
}

const alternativeNull = "0"

func (g *generator) expressionEnd(from int) int {
	depth, last := 0, from-1
	for j := from; j < len(g.toks); j++ {
		t := g.toks[j]
		depth += delta(t)
		if depth < 0 || depth == 0 && (t.kind == kindEnd || t.kind == kindNewline) {
			break
		}
		if t.kind != kindNewline {
			last = j
		}
	}
	return last
}

func delta(t token) int {
	if t.kind == kindOpen {
		return 1
	}
	if t.kind == kindClose {
		return -1
	}
	return 0
}

func (g *generator) conditionAt(i int) {
	t := g.toks[i]
	if t.kind != kindWord || !g.fam.CondWords[t.text] {
		return
	}
	switch {
	case g.fam.ColonBody:
		g.colonCondition(i)
	case g.fam.NotClose == "":
		g.prefixCondition(i)
	default:
		g.parenCondition(i)
	}
}

func (g *generator) colonCondition(i int) {
	depth := 0
	for j := i + 1; j < len(g.toks); j++ {
		depth += delta(g.toks[j])
		switch {
		case depth < 0 || depth == 0 && g.toks[j].kind == kindNewline:
			return
		case depth == 0 && g.toks[j].text == opColon:
			g.wrap(i+1, j-1)
			return
		}
	}
}

func (g *generator) wrap(from, to int) {
	if to < from {
		return
	}
	g.add(mutation.OpNegateCondition, from, to,
		mutation.Edit{Offset: g.toks[from].offset, Text: g.fam.NotOpen},
		mutation.Edit{Offset: g.end(to), Text: g.fam.NotClose})
}

func (g *generator) prefixCondition(i int) {
	next := g.at(i + 1)
	if next.kind == kindNewline || next.kind == kindEnd || g.fam.Negators[next.text] {
		return
	}
	g.add(mutation.OpNegateCondition, i+1, i+1, mutation.Edit{Offset: next.offset, Text: g.fam.NotOpen})
}

func (g *generator) parenCondition(i int) {
	open := i + 1
	if g.at(open).text != "(" {
		return
	}
	if closing := g.matching(open); closing > open+1 {
		g.wrap(open+1, closing-1)
	}
}

func (g *generator) matching(open int) int {
	depth := 0
	for j := open; j < len(g.toks); j++ {
		depth += delta(g.toks[j])
		if depth == 0 {
			return j
		}
	}
	return -1
}

func (g *generator) exceptAt(i int) {
	t := g.toks[i]
	if t.kind != kindWord || g.fam.Except == "" || t.text != g.fam.Except {
		return
	}
	if g.fam.ColonBody {
		g.pythonExcept(i)
		return
	}
	g.catchRethrow(i)
}

func (g *generator) pythonExcept(i int) {
	depth := 0
	for j := i + 1; j < len(g.toks); j++ {
		t := g.toks[j]
		depth += delta(t)
		if depth != 0 || t.text != opColon && t.text != wordAs {
			continue
		}
		if j == i+1 {
			g.add(mutation.OpErrorHandling, i, j, mutation.Edit{Offset: t.offset, Text: space + emptySet})
			return
		}
		g.add(mutation.OpErrorHandling, i+1, j-1, mutation.Edit{Offset: g.toks[i+1].offset, Length: g.end(j-1) - g.toks[i+1].offset, Text: emptySet})
		return
	}
}

func (g *generator) catchRethrow(i int) {
	body, binding := i+1, ""
	if g.at(i+1).text == "(" {
		closing := g.matching(i + 1)
		if closing < 0 {
			return
		}
		if inner := g.at(closing - 1); inner.kind == kindWord && closing-1 > i+1 {
			binding = inner.text
		}
		body = closing + 1
	}
	if g.at(body).text != openBody || strings.Contains(g.fam.Rethrow, "%s") && binding == "" {
		return
	}
	g.add(mutation.OpErrorHandling, i, body, mutation.Edit{Offset: g.end(body), Text: space + strings.Replace(g.fam.Rethrow, "%s", binding, 1)})
}

func (g *generator) statements() {
	start, depth := 0, 0
	for j, t := range g.toks {
		if !g.blockBrace(t) {
			depth += delta(t)
		}
		if depth > 0 || !g.statementEnd(t) {
			continue
		}
		g.statement(start, j-1)
		start = j + 1
	}
	g.statement(start, len(g.toks)-1)
}

func (g *generator) blockBrace(t token) bool {
	return g.fam.Noop == "" && (t.text == openBody || t.text == closeBody)
}

func (g *generator) statementEnd(t token) bool {
	if g.fam.Noop == "" {
		return t.kind == kindEnd || g.blockBrace(t)
	}
	return t.kind == kindNewline || t.kind == kindEnd
}

func (g *generator) statement(from, to int) {
	for from <= to && g.toks[from].kind == kindNewline {
		from++
	}
	if from > to || !g.removable(from, to) {
		return
	}
	if g.fam.Noop == "" && g.at(to+1).kind != kindEnd {
		return
	}
	g.add(mutation.OpRemoveStatement, from, to, mutation.Edit{Offset: g.toks[from].offset, Length: g.end(to) - g.toks[from].offset, Text: g.fam.Noop})
}

func (g *generator) removable(from, to int) bool {
	first, last := g.toks[from], g.toks[to]
	if first.kind != kindWord || g.fam.Keywords[first.text] || last.text == opColon || last.text == openBody {
		return false
	}
	return g.fam.Noop != "" || first.line == last.line
}
