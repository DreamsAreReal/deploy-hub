package gotmpl

import (
	"slices"
	"strconv"
	"strings"
	"text/template/parse"
)

const (
	unseen = iota
	covered
	exhausted
)

type choice struct {
	node parse.Node
	alt  int
}

type expander struct {
	trees    map[string]named
	seen     map[choice]int
	progress int
	out      strings.Builder
	spans    []span
	markup   markup
	stack    []string
	counter  int
}

func (e *expander) walk(n parse.Node, src *Source) {
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			e.walk(c, src)
		}
	case *parse.TextNode:
		e.emit(string(n.Text), src, int(n.Pos), len(n.Text))
	case *parse.ActionNode:
		e.action(n, src)
	case *parse.TemplateNode:
		e.call(n)
	case *parse.IfNode:
		e.branch(n, chain(&n.BranchNode, ifBranch), src)
	case *parse.WithNode:
		e.branch(n, chain(&n.BranchNode, withBranch), src)
	case *parse.RangeNode:
		e.branch(n, [][]*parse.ListNode{{n.List, n.List}, orEmpty(n.ElseList)}, src)
	}
}

func (e *expander) emit(text string, src *Source, pos, size int) {
	e.spans = append(e.spans, span{out: e.out.Len(), src: pos, size: size, file: src})
	e.out.WriteString(text)
	e.markup.feed(text)
}

func (e *expander) action(n *parse.ActionNode, src *Source) {
	if len(n.Pipe.Decl) > 0 || !e.markup.takesValue() {
		return
	}
	e.counter++
	text := strconv.Itoa(e.counter)
	e.emit(text, src, int(n.Pos), 0)
}

func (e *expander) call(n *parse.TemplateNode) {
	if slices.Contains(e.stack, n.Name) {
		return
	}
	target := e.trees[n.Name]
	e.stack = append(e.stack, n.Name)
	e.walk(target.tree.Root, target.src)
	e.stack = e.stack[:len(e.stack)-1]
}

func (e *expander) branch(n parse.Node, alts [][]*parse.ListNode, src *Source) {
	c := e.pick(n, alts)
	before := e.progress
	for _, list := range alts[c.alt] {
		e.walk(list, src)
	}
	e.settle(c, before)
}

func (e *expander) pick(n parse.Node, alts [][]*parse.ListNode) choice {
	best := choice{n, 0}
	for i := range alts {
		if c := (choice{n, i}); e.seen[c] < e.seen[best] {
			best = c
		}
	}
	if e.seen[best] == unseen {
		e.seen[best] = covered
		e.progress++
	}
	return best
}

func (e *expander) settle(c choice, before int) {
	if e.progress == before && e.seen[c] == covered {
		e.seen[c] = exhausted
		e.progress++
	}
}

func ifBranch(n parse.Node) *parse.BranchNode {
	if i, ok := n.(*parse.IfNode); ok {
		return &i.BranchNode
	}
	return nil
}

func withBranch(n parse.Node) *parse.BranchNode {
	if w, ok := n.(*parse.WithNode); ok {
		return &w.BranchNode
	}
	return nil
}

func chain(b *parse.BranchNode, nested func(parse.Node) *parse.BranchNode) [][]*parse.ListNode {
	var alts [][]*parse.ListNode
	for b != nil {
		alts = append(alts, []*parse.ListNode{b.List})
		b = elseOf(b, nested, &alts)
	}
	return alts
}

func elseOf(b *parse.BranchNode, nested func(parse.Node) *parse.BranchNode, alts *[][]*parse.ListNode) *parse.BranchNode {
	if b.ElseList != nil && len(b.ElseList.Nodes) == 1 {
		if next := nested(b.ElseList.Nodes[0]); next != nil {
			return next
		}
	}
	*alts = append(*alts, orEmpty(b.ElseList))
	return nil
}

func orEmpty(list *parse.ListNode) []*parse.ListNode {
	if list == nil {
		return nil
	}
	return []*parse.ListNode{list}
}
