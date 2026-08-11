package graph

import (
	"strings"

	"github.com/agentic-research/mache/api"
)

// schemaLevel is a compiled representation of one level in the schema tree.
type schemaLevel struct {
	nameRaw    string
	selector   string
	isStatic   bool
	staticName string
	children   []*schemaLevel
	files      []api.Leaf
	depth      int
}

// compileLevels compiles a Topology into a tree of schemaLevel nodes.
// Used by SQLiteGraph, WritableGraph, and NodesTableReader as the shared
// in-memory schema representation.
func compileLevels(schema *api.Topology) []*schemaLevel {
	var out []*schemaLevel
	for _, node := range schema.Nodes {
		out = append(out, compileOneLevel(node, 0))
	}
	return out
}

func compileOneLevel(node api.Node, depth int) *schemaLevel {
	l := &schemaLevel{
		nameRaw:  node.Name,
		selector: node.Selector,
		files:    node.Files,
		depth:    depth,
	}
	if !strings.Contains(node.Name, "{{") {
		l.isStatic = true
		l.staticName = node.Name
	}
	for _, child := range node.Children {
		l.children = append(l.children, compileOneLevel(child, depth+1))
	}
	return l
}

// walkSchemaLevels walks compiled schema levels to find the level and optional
// leaf matching the given path segments. Shared by SQLiteGraph and WritableGraph.
//
// When descending past a level with multiple children, prefer the static
// child whose name matches the path segment (e.g. go-schema's "functions" /
// "methods" / "types" siblings under each package). Fall back to the first
// dynamic (template-named) child when no static name matches.
func walkSchemaLevels(levels []*schemaLevel, segments []string) (*schemaLevel, *api.Leaf) {
	if len(segments) == 0 {
		return nil, nil
	}

	var root *schemaLevel
	for _, l := range levels {
		if l.isStatic && l.staticName == segments[0] {
			root = l
			break
		}
	}
	if root == nil {
		return nil, nil
	}
	if len(segments) == 1 {
		return root, nil
	}

	current := root
	for i := 1; i < len(segments); i++ {
		seg := segments[i]

		// Check if this segment matches a file at the current level
		for j := range current.files {
			fname := current.files[j].Name
			if !strings.Contains(fname, "{{") && fname == seg {
				return current, &current.files[j]
			}
		}

		if len(current.children) == 0 {
			return nil, nil
		}
		current = pickChildLevel(current.children, seg)
		if current == nil {
			return nil, nil
		}
	}

	return current, nil
}

// pickChildLevel selects the schema child that matches the given path segment.
// Returns the matching static child if one exists, otherwise the first dynamic
// (template-named) child, otherwise the first child as a last-resort fallback.
// Returns nil only when the input slice is empty.
func pickChildLevel(children []*schemaLevel, seg string) *schemaLevel {
	if len(children) == 0 {
		return nil
	}
	for _, c := range children {
		if c.isStatic && c.staticName == seg {
			return c
		}
	}
	for _, c := range children {
		if !c.isStatic {
			return c
		}
	}
	return children[0]
}
