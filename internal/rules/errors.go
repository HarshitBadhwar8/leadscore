// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadError is one problem found while compiling a rubric: the line, the
// field's path in the file (for example `derive.tier.rules[2].when`), and what
// is wrong. The caller adds the file name.
type LoadError struct {
	Line  int
	Field string
	Msg   string
}

func (e LoadError) Error() string {
	var b strings.Builder
	if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Field != "" {
		b.WriteString(e.Field + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// LoadErrors is every problem Compile found, in file order. Compile keeps going
// after a problem where it can, so `rules check` reports them all at once.
type LoadErrors []LoadError

func (es LoadErrors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// at is a position in the file: a node and its dotted path.
type at struct {
	n    *yaml.Node
	path string
}

func (a at) line() int {
	if a.n == nil {
		return 0
	}
	return a.n.Line
}

func (a at) key(k string) string {
	if a.path == "" {
		return k
	}
	return a.path + "." + k
}

func (a at) index(i int) string { return a.path + "[" + strconv.Itoa(i) + "]" }

// deref follows YAML aliases (`*name`) to the node they name.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// pair is one key and value of a mapping, in file order.
type pair struct {
	key   string
	keyN  *yaml.Node
	value *yaml.Node
}

// pairs returns a mapping's entries in file order.
func pairs(n *yaml.Node) []pair {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := deref(n.Content[i])
		out = append(out, pair{key: k.Value, keyN: k, value: deref(n.Content[i+1])})
	}
	return out
}

func isNull(n *yaml.Node) bool {
	n = deref(n)
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

func kindName(n *yaml.Node) string {
	switch deref(n).Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		if isNull(n) {
			return "empty"
		}
		return "a single value"
	}
	return "unknown"
}
