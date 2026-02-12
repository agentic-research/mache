package ingest

import (
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
)

// SitterWalker implements Walker for Tree-sitter parsed code.
type SitterWalker struct{}

func NewSitterWalker() *SitterWalker {
	return &SitterWalker{}
}

// SitterRoot encapsulates the necessary context for querying a Tree-sitter tree.
// It includes the root node, the source code (for extracting content), and the language (for compiling the query).
type SitterRoot struct {
	Node   *sitter.Node
	Source []byte
	Lang   *sitter.Language
}

// Query implements Walker.
func (w *SitterWalker) Query(root any, selector string) ([]Match, error) {
	sr, ok := root.(SitterRoot)
	if !ok {
		// Also support *SitterRoot just in case
		if ptr, ok := root.(*SitterRoot); ok {
			sr = *ptr
		} else {
			return nil, fmt.Errorf("root must be SitterRoot, got %T", root)
		}
	}

	// "$" is a passthrough selector — returns the root itself with empty values.
	// Used for grouping nodes (like "functions", "types") that use literal names.
	if selector == "$" {
		return []Match{&sitterMatch{
			values: make(map[string]string),
			scope:  sr.Node,
			root:   sr,
		}}, nil
	}

	// Compile the query
	q, err := sitter.NewQuery([]byte(selector), sr.Lang)
	if err != nil {
		return nil, fmt.Errorf("invalid query '%s': %w", selector, err)
	}
	defer q.Close()

	// Execute query
	qc := sitter.NewQueryCursor()
	defer qc.Close()

	qc.Exec(q, sr.Node)

	var matches []Match
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		// Convert captures to map
		vals := make(map[string]string)
		var scope *sitter.Node

		for _, c := range m.Captures {
			// Get capture name
			name := q.CaptureNameForId(c.Index)

			if name == "scope" {
				scope = c.Node
			}

			// Extract content from source
			start := c.Node.StartByte()
			end := c.Node.EndByte()

			// Safety check
			if start < uint32(len(sr.Source)) && end <= uint32(len(sr.Source)) {
				vals[name] = string(sr.Source[start:end])
			} else {
				vals[name] = "" // Should not happen if source matches tree
			}
		}
		matches = append(matches, &sitterMatch{
			values: vals,
			scope:  scope,
			root:   sr,
		})
	}

	return matches, nil
}

type sitterMatch struct {
	values map[string]string
	scope  *sitter.Node
	root   SitterRoot
}

// Values implements Match.
func (m *sitterMatch) Values() map[string]any {
	result := make(map[string]any, len(m.values))
	for k, v := range m.values {
		result[k] = v
	}
	return result
}

// Context implements Match.
func (m *sitterMatch) Context() any {
	if m.scope != nil {
		return SitterRoot{
			Node:   m.scope,
			Source: m.root.Source,
			Lang:   m.root.Lang,
		}
	}
	return nil
}
