package ingest

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJsonWalker(t *testing.T) {
	input := `
{
  "users": [
    {"name": "Alice", "role": "admin"},
    {"name": "Bob", "role": "user"}
  ],
  "meta": {
    "version": "1.0"
  }
}
`
	var data interface{}
	err := json.Unmarshal([]byte(input), &data)
	require.NoError(t, err)

	w := NewJsonWalker()

	t.Run("select list of objects", func(t *testing.T) {
		matches, err := w.Query(data, "$.users[*]")
		require.NoError(t, err)
		assert.Len(t, matches, 2)

		assert.Equal(t, map[string]any{"name": "Alice", "role": "admin"}, matches[0].Values())
		assert.Equal(t, map[string]any{"name": "Bob", "role": "user"}, matches[1].Values())
	})

	t.Run("select single object", func(t *testing.T) {
		matches, err := w.Query(data, "$.meta")
		require.NoError(t, err)
		assert.Len(t, matches, 1)
		assert.Equal(t, map[string]any{"version": "1.0"}, matches[0].Values())
	})

	t.Run("select primitive", func(t *testing.T) {
		matches, err := w.Query(data, "$.meta.version")
		require.NoError(t, err)
		assert.Len(t, matches, 1)
		// For primitive, we decide on a convention. Let's use "value" key.
		assert.Equal(t, map[string]any{"value": "1.0"}, matches[0].Values())
	})
}
