package ingest

import (
	"fmt"
	"reflect"

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
func (m *jsonMatch) Values() map[string]any {
	switch v := m.value.(type) {
	case map[string]any:
		return v // preserve nesting
	default:
		// Fallback: check for other map types via reflection
		val := reflect.ValueOf(m.value)
		if val.Kind() == reflect.Ptr {
			val = val.Elem()
		}
		if val.Kind() == reflect.Map {
			out := make(map[string]any, val.Len())
			for _, k := range val.MapKeys() {
				out[fmt.Sprint(k.Interface())] = val.MapIndex(k).Interface()
			}
			return out
		}
		return map[string]any{"value": v}
	}
}

// Context implements Match.
func (m *jsonMatch) Context() any {
	return m.value
}
