package mutation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

const (
	OpNegateCondition = "negate-condition"
	OpBoundary        = "comparison-boundary"
	OpComparison      = "comparison-negation"
	OpArithmetic      = "arithmetic"
	OpLogical         = "logical"
	OpRemoveNegation  = "remove-negation"
	OpReturnValue     = "return-value"
	OpRemoveStatement = "remove-statement"
	OpErrorHandling   = "error-handling"
	OpIncDec          = "increment-decrement"

	identNil   = "nil"
	identTrue  = "true"
	identFalse = "false"
	identErr   = "err"
	oneValue   = "1"
	mutatedStr = `"mutated"`
)

var (
	boundary = map[token.Token]token.Token{token.LSS: token.LEQ, token.LEQ: token.LSS, token.GTR: token.GEQ, token.GEQ: token.GTR}
	negation = map[token.Token]token.Token{
		token.EQL: token.NEQ, token.NEQ: token.EQL, token.LSS: token.GEQ,
		token.LEQ: token.GTR, token.GTR: token.LEQ, token.GEQ: token.LSS,
	}
	arithmetic = map[token.Token]token.Token{
		token.ADD: token.SUB, token.SUB: token.ADD, token.MUL: token.QUO, token.QUO: token.MUL, token.REM: token.MUL,
		token.ADD_ASSIGN: token.SUB_ASSIGN, token.SUB_ASSIGN: token.ADD_ASSIGN, token.MUL_ASSIGN: token.QUO_ASSIGN, token.QUO_ASSIGN: token.MUL_ASSIGN,
	}
	logical  = map[token.Token]token.Token{token.LAND: token.LOR, token.LOR: token.LAND}
	incDec   = map[token.Token]token.Token{token.INC: token.DEC, token.DEC: token.INC}
	numerics = map[string]bool{
		"int": true, "int8": true, "int16": true, "int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true,
		"uint32": true, "uint64": true, "uintptr": true, "float32": true, "float64": true, "byte": true, "rune": true,
	}
)

type edit struct {
	offset int
	length int
	text   string
}

type Mutant struct {
	File     string
	Line     int
	Operator string
	Original string
	Mutated  string
	edits    []edit
}

func (m Mutant) Apply(src []byte) []byte {
	edits := append([]edit(nil), m.edits...)
	sort.Slice(edits, func(i, j int) bool { return edits[i].offset > edits[j].offset })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		tail := append([]byte(e.text), out[e.offset+e.length:]...)
		out = append(out[:e.offset], tail...)
	}
	return out
}

type finder struct {
	fset  *token.FileSet
	src   []byte
	file  string
	out   []Mutant
	stack []ast.Node
}

func Points(path string, src []byte) ([]Mutant, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	fd := &finder{fset: fset, src: src, file: path}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			ast.Inspect(fn, fd.visit)
		}
	}
	return fd.out, nil
}

func (fd *finder) visit(n ast.Node) bool {
	if n == nil {
		fd.stack = fd.stack[:len(fd.stack)-1]
		return true
	}
	fd.stack = append(fd.stack, n)
	switch v := n.(type) {
	case *ast.BinaryExpr:
		fd.binary(v)
	case *ast.UnaryExpr:
		fd.unary(v)
	case *ast.IfStmt:
		fd.condition(v.Cond)
		fd.errorHandling(v.Cond)
	case *ast.ForStmt:
		fd.condition(v.Cond)
	case *ast.ReturnStmt:
		fd.returns(v)
	case *ast.ExprStmt:
		fd.add(OpRemoveStatement, v, edit{offset: fd.off(v.Pos()), length: fd.span(v), text: ""})
	case *ast.IncDecStmt:
		fd.swapToken(OpIncDec, v, v.TokPos, v.Tok, incDec)
	case *ast.AssignStmt:
		fd.swapToken(OpArithmetic, v, v.TokPos, v.Tok, arithmetic)
	}
	return true
}

func (fd *finder) off(p token.Pos) int {
	return fd.fset.Position(p).Offset
}

func (fd *finder) span(n ast.Node) int {
	return fd.off(n.End()) - fd.off(n.Pos())
}

func (fd *finder) text(n ast.Node) string {
	return string(fd.src[fd.off(n.Pos()):fd.off(n.End())])
}

func (fd *finder) add(op string, at ast.Node, edits ...edit) {
	m := Mutant{File: fd.file, Line: fd.fset.Position(at.Pos()).Line, Operator: op, Original: fd.text(at), edits: edits}
	m.Mutated = string(Mutant{edits: shift(edits, fd.off(at.Pos()))}.Apply([]byte(m.Original)))
	fd.out = append(fd.out, m)
}

func shift(edits []edit, by int) []edit {
	out := make([]edit, 0, len(edits))
	for _, e := range edits {
		out = append(out, edit{offset: e.offset - by, length: e.length, text: e.text})
	}
	return out
}

func (fd *finder) swapToken(op string, at ast.Node, pos token.Pos, tok token.Token, table map[token.Token]token.Token) {
	if to, ok := table[tok]; ok {
		fd.add(op, at, edit{offset: fd.off(pos), length: len(tok.String()), text: to.String()})
	}
}

func (fd *finder) binary(b *ast.BinaryExpr) {
	if isStringLit(b.X) || isStringLit(b.Y) {
		return
	}
	fd.swapToken(OpBoundary, b, b.OpPos, b.Op, boundary)
	fd.swapToken(OpComparison, b, b.OpPos, b.Op, negation)
	fd.swapToken(OpArithmetic, b, b.OpPos, b.Op, arithmetic)
	fd.swapToken(OpLogical, b, b.OpPos, b.Op, logical)
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

func (fd *finder) unary(u *ast.UnaryExpr) {
	if u.Op == token.NOT {
		fd.add(OpRemoveNegation, u, edit{offset: fd.off(u.OpPos), length: 1, text: ""})
	}
}

func (fd *finder) condition(cond ast.Expr) {
	if cond == nil {
		return
	}
	fd.add(OpNegateCondition, cond,
		edit{offset: fd.off(cond.Pos()), text: "!("},
		edit{offset: fd.off(cond.End()), text: ")"},
	)
}

func (fd *finder) errorHandling(cond ast.Expr) {
	if mentionsErr(cond) {
		fd.add(OpErrorHandling, cond, edit{offset: fd.off(cond.Pos()), length: fd.span(cond), text: identFalse})
	}
}

func mentionsErr(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.HasSuffix(strings.ToLower(id.Name), identErr) {
			found = true
		}
		return !found
	})
	return found
}

func (fd *finder) results() *ast.FieldList {
	for i := len(fd.stack) - 1; i >= 0; i-- {
		switch fn := fd.stack[i].(type) {
		case *ast.FuncDecl:
			return fn.Type.Results
		case *ast.FuncLit:
			return fn.Type.Results
		}
	}
	return nil
}

func (fd *finder) returns(r *ast.ReturnStmt) {
	types := resultTypes(fd.results())
	if len(types) != len(r.Results) {
		return
	}
	for i, e := range r.Results {
		if repl, ok := replacement(types[i], fd.text(e)); ok {
			fd.add(OpReturnValue, e, edit{offset: fd.off(e.Pos()), length: fd.span(e), text: repl})
		}
	}
}

func resultTypes(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		for range max(1, len(f.Names)) {
			out = append(out, f.Type)
		}
	}
	return out
}

func replacement(typ ast.Expr, text string) (string, bool) {
	if id, ok := typ.(*ast.Ident); ok {
		return identReplacement(id.Name, text)
	}
	switch t := typ.(type) {
	case *ast.StarExpr, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return identNil, text != identNil
	case *ast.ArrayType:
		return identNil, t.Len == nil && text != identNil
	}
	return "", false
}

func identReplacement(typeName, text string) (string, bool) {
	switch {
	case typeName == "error" || typeName == "any":
		return identNil, text != identNil
	case typeName == "bool":
		return negateBool(text), true
	case typeName == "string":
		return "(" + text + ") + " + mutatedStr, true
	case numerics[typeName]:
		return "(" + text + ") + " + oneValue, true
	default:
		return "", false
	}
}

func negateBool(text string) string {
	switch text {
	case identTrue:
		return identFalse
	case identFalse:
		return identTrue
	default:
		return "!(" + text + ")"
	}
}
