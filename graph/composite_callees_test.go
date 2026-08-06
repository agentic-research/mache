package graph

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCallExtractor is a deterministic stand-in for tree-sitter that
// returns a hard-coded call list regardless of input bytes. Lets the
// cross-mount tests drive the resolution path without CGO.
func fakeCallExtractor(calls []QualifiedCall) CallExtractor {
	return func(_ []byte, _, _ string) ([]QualifiedCall, error) {
		return calls, nil
	}
}

// TestCompositeGraph_CrossMountCallees is the headline cross-repo
// behavior: a function in mount A calls a function defined in mount
// B, and find_callees on A's function returns the def from B
// prefix-rewritten for the composite namespace.
func TestCompositeGraph_CrossMountCallees(t *testing.T) {
	// auth has a Charge() that calls Validate() (defined in billing).
	auth := NewMemoryStore()
	auth.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&Node{
		ID:         "functions/Charge",
		Mode:       fs.ModeDir,
		Children:   []string{"functions/Charge/source"},
		Properties: map[string]json.RawMessage{"lang": []byte(`"go"`)},
	})
	auth.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	require.NoError(t, auth.AddDef("Charge", "functions/Charge"))

	// billing defines Validate.
	billing := NewMemoryStore()
	billing.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	billing.AddNode(&Node{ID: "functions/Validate/source", Mode: 0})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	// Wire a deterministic extractor that returns the calls we
	// expect tree-sitter to produce for Charge's source.
	cg.SetCallExtractor(fakeCallExtractor([]QualifiedCall{
		{Token: "Validate"},
	}))

	callees, err := cg.GetCallees("auth/functions/Charge")
	require.NoError(t, err)

	gotIDs := make([]string, len(callees))
	for i, c := range callees {
		gotIDs[i] = c.ID
	}
	assert.Contains(t, gotIDs, "billing/functions/Validate",
		"cross-mount callee must be returned with composite-namespaced ID")
}

// TestCompositeGraph_CrossMountCalleesPrefersLocal asserts that when
// the same token resolves in both the local mount and another, both
// results appear (local AND cross-mount). The local mount's result
// wins on dedupe order — no duplicate IDs in the merged response.
func TestCompositeGraph_CrossMountCalleesDedupes(t *testing.T) {
	// Both mounts define Validate; auth's Charge calls Validate.
	auth := NewMemoryStore()
	auth.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&Node{
		ID:         "functions/Charge",
		Mode:       fs.ModeDir,
		Children:   []string{"functions/Charge/source"},
		Properties: map[string]json.RawMessage{"lang": []byte(`"go"`)},
	})
	auth.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	auth.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	auth.AddNode(&Node{ID: "functions/Validate/source", Mode: 0})
	require.NoError(t, auth.AddDef("Charge", "functions/Charge"))
	require.NoError(t, auth.AddDef("Validate", "functions/Validate"))
	auth.SetCallExtractor(fakeCallExtractor([]QualifiedCall{{Token: "Validate"}}))

	billing := NewMemoryStore()
	billing.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	billing.AddNode(&Node{ID: "functions/Validate/source", Mode: 0})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))
	cg.SetCallExtractor(fakeCallExtractor([]QualifiedCall{{Token: "Validate"}}))

	callees, err := cg.GetCallees("auth/functions/Charge")
	require.NoError(t, err)

	gotIDs := make(map[string]int, len(callees))
	for _, c := range callees {
		gotIDs[c.ID]++
	}
	assert.Equal(t, 1, gotIDs["auth/functions/Validate"], "local mount's def appears exactly once")
	assert.Equal(t, 1, gotIDs["billing/functions/Validate"], "cross-mount def appears exactly once")
}

// TestCompositeGraph_CallExtractorPickerWinsOverGlobal pins the
// per-mount dispatch contract added for ADR-0012's CGO removal arc:
// when SetCallExtractorPicker returns a non-nil extractor, it
// overrides the global SetCallExtractor for cross-mount resolution
// against THAT local mount. The picker is consulted at extract time
// with the routed local Graph.
func TestCompositeGraph_CallExtractorPickerWinsOverGlobal(t *testing.T) {
	auth := NewMemoryStore()
	auth.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&Node{
		ID:         "functions/Charge",
		Mode:       fs.ModeDir,
		Children:   []string{"functions/Charge/source"},
		Properties: map[string]json.RawMessage{"lang": []byte(`"go"`)},
	})
	auth.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	require.NoError(t, auth.AddDef("Charge", "functions/Charge"))

	billing := NewMemoryStore()
	billing.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	billing.AddNode(&Node{ID: "functions/Validate/source", Mode: 0})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	// Global extractor returns "WrongCall" — would NOT resolve.
	cg.SetCallExtractor(fakeCallExtractor([]QualifiedCall{{Token: "WrongCall"}}))

	// Picker returns the right extractor for this local mount.
	// Picker getting called proves the per-mount dispatch path runs.
	var pickerCalled int
	cg.SetCallExtractorPicker(func(local Graph) CallExtractor {
		pickerCalled++
		return fakeCallExtractor([]QualifiedCall{{Token: "Validate"}})
	})

	callees, err := cg.GetCallees("auth/functions/Charge")
	require.NoError(t, err)
	assert.Equal(t, 1, pickerCalled, "picker must be consulted on cross-mount extract")

	gotIDs := make([]string, len(callees))
	for i, c := range callees {
		gotIDs[i] = c.ID
	}
	assert.Contains(t, gotIDs, "billing/functions/Validate",
		"picker's extractor must win over the global; Validate must resolve")
}

// TestCompositeGraph_CallExtractorPickerNilFallsThrough pins the
// fallback contract: when the picker returns nil, GetCallees uses
// the global extractor set via SetCallExtractor. Lets callers wire
// a picker that opts out for some mounts while keeping the global
// path for others.
func TestCompositeGraph_CallExtractorPickerNilFallsThrough(t *testing.T) {
	auth := NewMemoryStore()
	auth.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&Node{
		ID:         "functions/Charge",
		Mode:       fs.ModeDir,
		Children:   []string{"functions/Charge/source"},
		Properties: map[string]json.RawMessage{"lang": []byte(`"go"`)},
	})
	auth.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	require.NoError(t, auth.AddDef("Charge", "functions/Charge"))

	billing := NewMemoryStore()
	billing.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	billing.AddNode(&Node{ID: "functions/Validate/source", Mode: 0})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	// Global extractor returns the right call.
	cg.SetCallExtractor(fakeCallExtractor([]QualifiedCall{{Token: "Validate"}}))

	// Picker returns nil — fall through to global.
	cg.SetCallExtractorPicker(func(local Graph) CallExtractor { return nil })

	callees, err := cg.GetCallees("auth/functions/Charge")
	require.NoError(t, err)

	gotIDs := make([]string, len(callees))
	for i, c := range callees {
		gotIDs[i] = c.ID
	}
	assert.Contains(t, gotIDs, "billing/functions/Validate",
		"picker returning nil must fall through to the global extractor")
}

// TestCompositeGraph_CrossMountCalleesNoExtractor proves the feature
// is opt-in: without SetCallExtractor, GetCallees only consults the
// local mount (the previously-shipped behavior).
func TestCompositeGraph_CrossMountCalleesNoExtractor(t *testing.T) {
	auth := NewMemoryStore()
	auth.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&Node{
		ID:         "functions/Charge",
		Mode:       fs.ModeDir,
		Children:   []string{"functions/Charge/source"},
		Properties: map[string]json.RawMessage{"lang": []byte(`"go"`)},
	})
	auth.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	require.NoError(t, auth.AddDef("Charge", "functions/Charge"))

	billing := NewMemoryStore()
	billing.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&Node{
		ID:   "functions/Validate",
		Mode: fs.ModeDir,
	})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))
	// Intentionally NOT calling cg.SetCallExtractor.

	callees, err := cg.GetCallees("auth/functions/Charge")
	require.NoError(t, err)

	for _, c := range callees {
		assert.NotEqual(t, "billing/functions/Validate", c.ID,
			"without an extractor, the composite must not reach into other mounts")
	}
}
