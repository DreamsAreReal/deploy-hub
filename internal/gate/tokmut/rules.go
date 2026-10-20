package tokmut

import (
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
)

type swap struct {
	op string
	to string
}

type Family struct {
	Name       string
	Swaps      map[string][]swap
	Negators   map[string]bool
	Null       string
	CondWords  map[string]bool
	ColonBody  bool
	NotOpen    string
	NotClose   string
	Noop       string
	Keywords   map[string]bool
	ReturnWord string
	Except     string
	Rethrow    string
}

const (
	opLT       = "<"
	opLE       = "<="
	opGT       = ">"
	opGE       = ">="
	opEQ       = "=="
	opNE       = "!="
	opStrictEQ = "==="
	opStrictNE = "!=="
	opAdd      = "+"
	opSub      = "-"
	opMul      = "*"
	opDiv      = "/"
	opMod      = "%"
	opAddTo    = "+="
	opSubFrom  = "-="
	opMulBy    = "*="
	opDivBy    = "/="
	opFloorDiv = "//"
	opPow      = "**"
	opAnd      = "&&"
	opOr       = "||"
	opInc      = "++"
	opDec      = "--"
	opNot      = "!"
	pyAnd      = "and"
	pyOr       = "or"
	pyTrue     = "True"
	pyFalse    = "False"
	lowerTrue  = "true"
	lowerFalse = "false"
	wordReturn = "return"
	wordCatch  = "catch"
	bangOpen   = "!("
	parenClose = ")"
	shEq       = "-eq"
	shNe       = "-ne"
	shLt       = "-lt"
	shLe       = "-le"
	shGt       = "-gt"
	shGe       = "-ge"
)

var (
	boundary   = map[string]string{opLT: opLE, opLE: opLT, opGT: opGE, opGE: opGT}
	negation   = map[string]string{opEQ: opNE, opNE: opEQ, opLT: opGE, opLE: opGT, opGT: opLE, opGE: opLT, opStrictEQ: opStrictNE, opStrictNE: opStrictEQ}
	arithmetic = map[string]string{
		opAdd: opSub, opSub: opAdd, opMul: opDiv, opDiv: opMul, opMod: opMul, opAddTo: opSubFrom, opSubFrom: opAddTo, opMulBy: opDivBy, opDivBy: opMulBy,
		opFloorDiv: opDiv, opPow: opMul,
	}
	logical  = map[string]string{opAnd: opOr, opOr: opAnd}
	incDec   = map[string]string{opInc: opDec, opDec: opInc}
	booleans = map[string]swap{lowerTrue: {mutation.OpLogical, lowerFalse}, lowerFalse: {mutation.OpLogical, lowerTrue}}
)

func swaps(tables ...map[string]string) map[string][]swap {
	ops := []string{mutation.OpBoundary, mutation.OpComparison, mutation.OpArithmetic, mutation.OpLogical, mutation.OpIncDec}
	out := map[string][]swap{}
	for i, t := range tables {
		for from, to := range t {
			out[from] = append(out[from], swap{op: ops[i], to: to})
		}
	}
	return out
}

func words(m map[string][]swap, extra map[string]swap) map[string][]swap {
	for w, s := range extra {
		m[w] = append(m[w], s)
	}
	return m
}

func set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, i := range items {
		out[i] = true
	}
	return out
}

var Python = Family{
	Name: "python",
	Swaps: words(swaps(boundary, negation, arithmetic, logical, nil), map[string]swap{
		pyAnd: {mutation.OpLogical, pyOr}, pyOr: {mutation.OpLogical, pyAnd},
		pyTrue: {mutation.OpLogical, pyFalse}, pyFalse: {mutation.OpLogical, pyTrue},
	}),
	Negators:  set("not"),
	Null:      "None",
	CondWords: set("if", "elif", "while"),
	ColonBody: true,
	NotOpen:   "not (",
	NotClose:  ")",
	Noop:      "pass",
	Keywords: set("def", "class", "if", "elif", "else", "for", "while", "try", "except", "finally", "with", wordReturn,
		"import", "from", "pass", "break", "continue", "async", "global", "nonlocal", "assert", "yield", "lambda", "match", "case", "del"),
	ReturnWord: wordReturn,
	Except:     "except",
}

var JavaScript = Family{
	Name:      "javascript",
	Swaps:     words(swaps(boundary, negation, arithmetic, logical, incDec), booleans),
	Negators:  set(opNot),
	Null:      "null",
	CondWords: set("if", "while"),
	NotOpen:   bangOpen,
	NotClose:  parenClose,
	Keywords: set("if", "else", "for", "while", "do", "switch", "case", "default", "try", wordCatch, "finally", wordReturn,
		"break", "continue", "function", "class", "const", "let", "var", "import", "export", "async", "await", "yield", "new",
		"interface", "type", "enum", "namespace", "declare", "abstract", "public", "private", "protected", "static", "readonly"),
	ReturnWord: wordReturn,
	Except:     wordCatch,
	Rethrow:    "throw %s;",
}

var CSharp = Family{
	Name:      "csharp",
	Swaps:     words(swaps(boundary, negation, arithmetic, logical, incDec), booleans),
	Negators:  set(opNot),
	Null:      "default",
	CondWords: set("if", "while"),
	NotOpen:   bangOpen,
	NotClose:  parenClose,
	Keywords: set("if", "else", "for", "foreach", "while", "do", "switch", "case", "default", "try", wordCatch, "finally", wordReturn,
		"break", "continue", "using", "namespace", "class", "struct", "record", "interface", "enum", "public", "private",
		"protected", "internal", "static", "readonly", "const", "var", "new", "yield", "await", "lock", "fixed", "unsafe", "checked",
		"unchecked", "goto", "partial", "sealed", "abstract", "override", "virtual", "async", "void", "get", "set", "init"),
	ReturnWord: wordReturn,
	Except:     wordCatch,
	Rethrow:    "throw;",
}

var Shell = Family{
	Name: "shell",
	Swaps: words(swaps(nil, map[string]string{opEQ: opNE, opNE: opEQ, opAssign: opNE}, nil, logical, nil), map[string]swap{
		shEq: {mutation.OpComparison, shNe}, shNe: {mutation.OpComparison, shEq},
		shLt: {mutation.OpBoundary, shLe}, shLe: {mutation.OpBoundary, shLt},
		shGt: {mutation.OpBoundary, shGe}, shGe: {mutation.OpBoundary, shGt},
		lowerTrue: {mutation.OpLogical, lowerFalse}, lowerFalse: {mutation.OpLogical, lowerTrue},
	}),
	Negators:  set(opNot),
	Null:      "1",
	CondWords: set("if", "elif", "while", "until"),
	NotOpen:   "! ",
	Noop:      ":",
	Keywords: set("if", "then", "elif", "else", "fi", "for", "while", "until", "do", "done", "case", "esac", "function", wordReturn,
		"exit", "local", "readonly", "declare", "export", "set", "shift", "trap", "in", "select", "time"),
	ReturnWord: wordReturn,
}

func Points(name string, data []byte, fam Family) ([]mutation.Mutant, error) {
	toks, known, err := lexis.Lex(name, data)
	if err != nil || !known {
		return nil, err
	}
	g := &generator{fam: fam, file: name, src: data, toks: scan(data, masked(toks, len(data)))}
	g.run()
	return g.out, nil
}
