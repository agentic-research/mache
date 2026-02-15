package lattice

import (
	"encoding/json"
	"testing"
)

func FuzzInferFromRecords(f *testing.F) {
	// Seed corpus
	f.Add(`[{"name": "foo", "value": 1}, {"name": "bar", "value": 2}]`)
	f.Add(`[{"id": 1, "tags": ["a", "b"]}, {"id": 2, "tags": ["b"]}]`)
	f.Add(`[]`)

	f.Fuzz(func(t *testing.T, data string) {
		var records []any
		if err := json.Unmarshal([]byte(data), &records); err != nil {
			return // invalid JSON is not interesting for logic fuzzing
		}

		// Limit size to avoid timeouts during fuzzing
		if len(records) > 50 {
			records = records[:50]
		}

		inf := &Inferrer{Config: DefaultInferConfig()}
		// Use "fca" method as it uses the complex NextClosure algo
		inf.Config.Method = "fca"

		schema, err := inf.InferFromRecords(records)
		if err != nil {
			// It's okay to fail inference on garbage data, but not panic
			return
		}

		if schema == nil {
			t.Fatal("schema is nil")
		}
	})
}
