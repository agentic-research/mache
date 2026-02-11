package ingest

import (
	"fmt"

	"github.com/ohler55/ojg/jp"
)

// JsonWalker implements Walker for JSON-like data.
type JsonWalker struct{}

func NewJsonWalker() *JsonWalker {
	return &JsonWalker{}
}

// Query implements Walker.
func (w *JsonWalker) Query(root any, selector string) ([]Match, error) {
	// Parse JSONPath
	x, err := jp.ParseString(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid jsonpath '%s': %w", selector, err)
	}

	// Execute query
	results := x.Get(root)

	// Wrap results in Match
	matches := make([]Match, len(results))
	for i, r := range results {
		matches[i] = &jsonMatch{value: r}
	}

	return matches, nil
}

type jsonMatch struct {
	value any
}

// Values implements Match.
func (m *jsonMatch) Values() map[string]string {
	res := make(map[string]string)

	switch v := m.value.(type) {
	case map[string]any:
		for k, val := range v {
			res[k] = fmt.Sprintf("%v", val)
		}
	default:
		// For primitives, return as "value"
		res["value"] = fmt.Sprintf("%v", v)
	}
	return res
}

// Context implements Match.
func (m *jsonMatch) Context() any {
	return m.value
}
