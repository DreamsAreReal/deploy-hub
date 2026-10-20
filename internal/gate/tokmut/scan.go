package tokmut

import (
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
)

type kind int

const (
	kindWord kind = iota
	kindNumber
	kindOp
	kindOpen
	kindClose
	kindEnd
	kindNewline
	kindText
)

type token struct {
	kind   kind
	text   string
	offset int
	line   int
}

var operators = []string{
	opStrictEQ, opStrictNE, "**=", "//=", "<<=", ">>=",
	opEQ, opNE, opLE, opGE, opAnd, opOr, opInc, opDec, opAddTo, opSubFrom, opMulBy, opDivBy, "%=", opPow, opFloorDiv, "->", "=>", "<<", ">>", "::", "??",
	opLT, opGT, opAdd, opSub, opMul, opDiv, opMod, opNot, opAssign, "&", "|", "^", "~", "?", opColon, ".", ",", "@",
}

const (
	opens  = "([{"
	closes = ")]}"
	ends   = ";"
)

func masked(toks []lexis.Token, size int) []bool {
	mask := make([]bool, size)
	for _, t := range toks {
		if t.Note || strings.HasPrefix(t.Kind, stringKind) || strings.HasPrefix(t.Kind, commentKind) {
			for i := t.Offset; i < t.Offset+len(t.Text) && i < size; i++ {
				mask[i] = true
			}
		}
	}
	return mask
}

const (
	stringKind  = "LiteralString"
	commentKind = "Comment"
)

type scanner struct {
	src  []byte
	mask []bool
	pos  int
	line int
	out  []token
}

func scan(src []byte, mask []bool) []token {
	s := &scanner{src: src, mask: mask, line: 1}
	for range src {
		if s.pos >= len(s.src) {
			break
		}
		s.step()
	}
	return s.out
}

func (s *scanner) step() {
	c := s.src[s.pos]
	switch {
	case s.mask[s.pos]:
		s.skipMasked()
	case c == '\n':
		s.emit(kindNewline, 1)
		s.line++
	case c == ' ' || c == '\t' || c == '\r':
		s.pos++
	case isWordByte(c):
		s.emit(s.wordKind(), s.wordLen())
	default:
		s.symbol()
	}
}

func (s *scanner) skipMasked() {
	start := s.pos
	for s.pos < len(s.src) && s.mask[s.pos] {
		if s.src[s.pos] == '\n' {
			s.line++
		}
		s.pos++
	}
	s.out = append(s.out, token{kind: kindText, text: string(s.src[start:s.pos]), offset: start, line: s.line})
}

func (s *scanner) emit(k kind, n int) {
	s.out = append(s.out, token{kind: k, text: string(s.src[s.pos : s.pos+n]), offset: s.pos, line: s.line})
	s.pos += n
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func (s *scanner) wordLen() int {
	n := 0
	for s.pos+n < len(s.src) && !s.mask[s.pos+n] && isWordByte(s.src[s.pos+n]) {
		n++
	}
	return n
}

func (s *scanner) wordKind() kind {
	if c := s.src[s.pos]; c >= '0' && c <= '9' {
		return kindNumber
	}
	return kindWord
}

func (s *scanner) symbol() {
	c := s.src[s.pos]
	switch {
	case strings.IndexByte(opens, c) >= 0:
		s.emit(kindOpen, 1)
	case strings.IndexByte(closes, c) >= 0:
		s.emit(kindClose, 1)
	case strings.IndexByte(ends, c) >= 0:
		s.emit(kindEnd, 1)
	default:
		s.emit(kindOp, s.operatorLen())
	}
}

func (s *scanner) operatorLen() int {
	for _, op := range operators {
		end := s.pos + len(op)
		if end <= len(s.src) && string(s.src[s.pos:end]) == op && !s.anyMasked(s.pos, end) {
			return len(op)
		}
	}
	return 1
}

func (s *scanner) anyMasked(from, to int) bool {
	for i := from; i < to; i++ {
		if s.mask[i] {
			return true
		}
	}
	return false
}
