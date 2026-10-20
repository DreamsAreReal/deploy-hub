package compose

import (
	"go.yaml.in/yaml/v3"
)

const (
	mergeKey  = "<<"
	tagStr    = "!!str"
	tagMap    = "!!map"
	tagSeq    = "!!seq"
	pairWidth = 2
)

type entry struct {
	key   *yaml.Node
	value *yaml.Node
	index int
}

func entries(m *yaml.Node) []entry {
	out := make([]entry, 0, len(m.Content)/pairWidth)
	for i := range len(m.Content) / pairWidth {
		out = append(out, entry{key: m.Content[pairWidth*i], value: m.Content[pairWidth*i+1], index: pairWidth * i})
	}
	return out
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for _, e := range entries(m) {
		if e.key.Value == key {
			return e.value
		}
	}
	return nil
}

func has(m *yaml.Node, key string) bool {
	return get(m, key) != nil
}

func set(m *yaml.Node, key string, value *yaml.Node) {
	for _, e := range entries(m) {
		if e.key.Value == key {
			m.Content[e.index+1] = value
			return
		}
	}
	m.Content = append(m.Content, str(key), value)
}

func setDefault(m *yaml.Node, key string, value *yaml.Node) {
	if !has(m, key) {
		m.Content = append(m.Content, str(key), value)
	}
}

func remove(m *yaml.Node, key string) {
	for _, e := range entries(m) {
		if e.key.Value == key {
			m.Content = append(m.Content[:e.index], m.Content[e.index+pairWidth:]...)
			return
		}
	}
}

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: v}
}

func mapping(pairs ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap, Content: pairs}
}

func sequence(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq, Content: items}
}

func resolve(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		return resolve(n.Alias)
	}
	out := *n
	out.Anchor = ""
	out.Content = make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		out.Content = append(out.Content, resolve(c))
	}
	if out.Kind == yaml.MappingNode {
		expandMerge(&out)
	}
	return &out
}

func expandMerge(m *yaml.Node) {
	var own, merged []*yaml.Node
	for _, e := range entries(m) {
		if e.key.Value == mergeKey && e.key.Tag != tagStr {
			merged = append(merged, mergeSources(e.value)...)
			continue
		}
		own = append(own, e.key, e.value)
	}
	m.Content = own
	for _, src := range merged {
		for _, e := range entries(src) {
			setDefault(m, e.key.Value, e.value)
		}
	}
}

func mergeSources(v *yaml.Node) []*yaml.Node {
	if v.Kind == yaml.SequenceNode {
		return v.Content
	}
	return []*yaml.Node{v}
}
