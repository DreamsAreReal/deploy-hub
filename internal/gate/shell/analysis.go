package shell

import (
	"bytes"
	"slices"

	"mvdan.cc/sh/v3/syntax"
)

const mainFunction = "(main)"

type branch struct {
	arm       uint
	condition uint
	implicit  bool
}

type script struct {
	lines     []uint
	branches  []branch
	functions map[string]int
}

func parse(name string, data []byte) (*syntax.File, error) {
	return syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash)).Parse(bytes.NewReader(data), name)
}

func analyze(f *syntax.File) script {
	s := script{functions: map[string]int{}}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch v := n.(type) {
		case *syntax.Stmt:
			s.statement(v)
		case *syntax.IfClause:
			s.ifBranches(v)
		case *syntax.CaseClause:
			s.caseBranches(v)
		case *syntax.FuncDecl:
			s.functions[v.Name.Value] = complexity(v.Body)
		}
		return true
	})
	s.functions[mainFunction] = topLevelComplexity(f)
	slices.Sort(s.lines)
	return s
}

func (s *script) statement(st *syntax.Stmt) {
	switch st.Cmd.(type) {
	case *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause, *syntax.Block, *syntax.Subshell, *syntax.FuncDecl, *syntax.BinaryCmd:
		return
	}
	if line := st.Pos().Line(); !slices.Contains(s.lines, line) {
		s.lines = append(s.lines, line)
	}
}

func first(stmts []*syntax.Stmt) (uint, bool) {
	if len(stmts) == 0 {
		return 0, false
	}
	return stmts[0].Pos().Line(), true
}

func (s *script) ifBranches(c *syntax.IfClause) {
	cond, hasCond := first(c.Cond)
	then, hasThen := first(c.Then)
	if !hasCond || !hasThen {
		return
	}
	s.branches = append(s.branches, branch{arm: then, condition: cond})
	switch {
	case c.Else == nil:
		s.branches = append(s.branches, branch{arm: then, condition: cond, implicit: true})
	case len(c.Else.Cond) == 0:
		if arm, ok := first(c.Else.Then); ok {
			s.branches = append(s.branches, branch{arm: arm, condition: cond})
		}
	}
}

func (s *script) caseBranches(c *syntax.CaseClause) {
	for _, item := range c.Items {
		if arm, ok := first(item.Stmts); ok {
			s.branches = append(s.branches, branch{arm: arm})
		}
	}
}

func (b branch) covered(counts map[uint]int) bool {
	if b.implicit {
		return counts[b.condition] > counts[b.arm]
	}
	return counts[b.arm] > 0
}

type scorer struct {
	total int
}

func complexity(body *syntax.Stmt) int {
	sc := &scorer{}
	sc.stmt(body, 0)
	return sc.total
}

func topLevelComplexity(f *syntax.File) int {
	sc := &scorer{}
	for _, st := range f.Stmts {
		if _, isFunc := st.Cmd.(*syntax.FuncDecl); !isFunc {
			sc.stmt(st, 0)
		}
	}
	return sc.total
}

func (sc *scorer) stmts(list []*syntax.Stmt, nesting int) {
	for _, st := range list {
		sc.stmt(st, nesting)
	}
}

func (sc *scorer) stmt(st *syntax.Stmt, nesting int) {
	switch c := st.Cmd.(type) {
	case *syntax.IfClause:
		sc.total += 1 + nesting
		sc.ifChain(c, nesting)
	case *syntax.WhileClause:
		sc.total += 1 + nesting
		sc.stmts(c.Cond, nesting)
		sc.stmts(c.Do, nesting+1)
	case *syntax.ForClause:
		sc.total += 1 + nesting
		sc.stmts(c.Do, nesting+1)
	case *syntax.CaseClause:
		sc.total += 1 + nesting
		for _, item := range c.Items {
			sc.stmts(item.Stmts, nesting+1)
		}
	case *syntax.BinaryCmd:
		sc.binary(c, nesting)
	case *syntax.Block:
		sc.stmts(c.Stmts, nesting)
	case *syntax.Subshell:
		sc.stmts(c.Stmts, nesting)
	case *syntax.FuncDecl:
		sc.stmt(c.Body, nesting+1)
	}
}

func (sc *scorer) ifChain(c *syntax.IfClause, nesting int) {
	sc.stmts(c.Cond, nesting)
	sc.stmts(c.Then, nesting+1)
	if c.Else == nil {
		return
	}
	sc.total++
	if len(c.Else.Cond) > 0 {
		sc.ifChain(c.Else, nesting)
		return
	}
	sc.stmts(c.Else.Then, nesting+1)
}

func (sc *scorer) binary(c *syntax.BinaryCmd, nesting int) {
	if c.Op == syntax.AndStmt || c.Op == syntax.OrStmt {
		if parent, ok := c.X.Cmd.(*syntax.BinaryCmd); !ok || parent.Op != c.Op {
			sc.total++
		}
	}
	sc.stmt(c.X, nesting)
	sc.stmt(c.Y, nesting)
}
