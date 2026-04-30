package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompositeGraph_DefsMapFederates asserts that DefsMap aggregates
// token → dir IDs across every mount, prefixing each ID with the
// mount name. A token defined in two mounts returns both, prefixed
// correctly.
func TestCompositeGraph_DefsMapFederates(t *testing.T) {
	auth := NewMemoryStore()
	require.NoError(t, auth.AddDef("Validate", "functions/Validate"))
	require.NoError(t, auth.AddDef("AuthOnly", "functions/AuthOnly"))

	billing := NewMemoryStore()
	require.NoError(t, billing.AddDef("Validate", "functions/Validate")) // shared name
	require.NoError(t, billing.AddDef("Charge", "functions/Charge"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	defs := cg.DefsMap()

	// Shared token returns prefixed dir IDs from both mounts.
	require.Contains(t, defs, "Validate")
	assert.ElementsMatch(t,
		[]string{"auth/functions/Validate", "billing/functions/Validate"},
		defs["Validate"],
		"shared token federates across mounts with prefixes")

	// Per-mount tokens are prefixed by their mount of origin.
	require.Contains(t, defs, "AuthOnly")
	assert.Equal(t, []string{"auth/functions/AuthOnly"}, defs["AuthOnly"])
	require.Contains(t, defs, "Charge")
	assert.Equal(t, []string{"billing/functions/Charge"}, defs["Charge"])
}

// TestCompositeGraph_DefsMapSkipsNonProvider ensures sub-graphs that
// don't expose a DefsMap method are silently skipped (not panicked).
// The composite still returns whatever the other mounts have.
func TestCompositeGraph_DefsMapSkipsNonProvider(t *testing.T) {
	auth := NewMemoryStore()
	require.NoError(t, auth.AddDef("Validate", "functions/Validate"))

	cg := NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("opaque", &noDefsGraph{})) // no DefsMap

	defs := cg.DefsMap()
	require.Contains(t, defs, "Validate")
	assert.Equal(t, []string{"auth/functions/Validate"}, defs["Validate"])
}

// noDefsGraph implements just enough of Graph to be Mount-able. It
// intentionally does NOT implement DefsMap so the composite must
// skip it gracefully.
type noDefsGraph struct{}

func (*noDefsGraph) GetNode(string) (*Node, error)                  { return nil, ErrNotFound }
func (*noDefsGraph) ListChildren(string) ([]string, error)          { return nil, nil }
func (*noDefsGraph) ListChildStats(string) ([]NodeStat, error)      { return nil, nil }
func (*noDefsGraph) ReadContent(string, []byte, int64) (int, error) { return 0, nil }
func (*noDefsGraph) GetCallers(string) ([]*Node, error)             { return nil, nil }
func (*noDefsGraph) GetCallees(string) ([]*Node, error)             { return nil, nil }
func (*noDefsGraph) Invalidate(string)                              {}
func (*noDefsGraph) Act(string, string, string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}
