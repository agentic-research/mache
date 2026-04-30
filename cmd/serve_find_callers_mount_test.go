package cmd

import (
	"context"
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopedCallerRow mirrors the JSON shape the handler produces when
// any result carries a mount prefix.
type scopedCallerRow struct {
	Path  string `json:"path"`
	Mount string `json:"mount,omitempty"`
}

// twoMountStores builds two MemoryStores each with one caller of the
// shared token "Validate" — one in "auth", one in "billing". Mirrors
// the cross-repo refs use case mache-iegm tracks.
func twoMountStores(t *testing.T) (*graph.MemoryStore, *graph.MemoryStore) {
	t.Helper()
	auth := graph.NewMemoryStore()
	auth.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&graph.Node{ID: "functions/AuthCaller", Mode: fs.ModeDir})
	auth.AddNode(&graph.Node{ID: "functions/AuthCaller/source", Mode: 0, Data: []byte("func AuthCaller() { Validate() }")})
	require.NoError(t, auth.AddRef("Validate", "functions/AuthCaller/source"))

	billing := graph.NewMemoryStore()
	billing.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&graph.Node{ID: "functions/Charge", Mode: fs.ModeDir})
	billing.AddNode(&graph.Node{ID: "functions/Charge/source", Mode: 0, Data: []byte("func Charge() { Validate() }")})
	require.NoError(t, billing.AddRef("Validate", "functions/Charge/source"))
	return auth, billing
}

// TestFindCallers_AnnotatesMountOnComposite asserts that when the
// active graph is a CompositeGraph and callers come from multiple
// mounts, the response surfaces the mount prefix per result rather
// than burying it in the path string.
func TestFindCallers_AnnotatesMountOnComposite(t *testing.T) {
	auth, billing := twoMountStores(t)
	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	handler := makeFindCallersHandler(cg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"token": "Validate"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Callers []scopedCallerRow `json:"callers"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Len(t, resp.Callers, 2)

	byMount := make(map[string]string, len(resp.Callers))
	for _, c := range resp.Callers {
		byMount[c.Mount] = c.Path
	}
	require.Contains(t, byMount, "auth", "expected at least one caller from auth mount")
	require.Contains(t, byMount, "billing", "expected at least one caller from billing mount")
	assert.Equal(t, "auth/functions/AuthCaller/source", byMount["auth"])
	assert.Equal(t, "billing/functions/Charge/source", byMount["billing"])
}

// TestFindCallers_NonCompositeKeepsLegacyShape ensures a non-composite
// graph still returns the historical []string response — agents that
// hardcode the old shape don't break.
func TestFindCallers_NonCompositeKeepsLegacyShape(t *testing.T) {
	auth, _ := twoMountStores(t)
	handler := makeFindCallersHandler(auth)
	res, err := handler(context.Background(), makeRequest(map[string]any{"token": "Validate"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	// Legacy shape: bare []string.
	var paths []string
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &paths))
	assert.Equal(t, []string{"functions/AuthCaller/source"}, paths)
}

// TestFindCallers_CompositeNoCallersKeepsLegacyShape covers the path
// where the composite is in play but the token has no callers — the
// handler falls through to the existing "[]" empty-array response.
func TestFindCallers_CompositeNoCallersKeepsLegacyShape(t *testing.T) {
	auth, billing := twoMountStores(t)
	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	handler := makeFindCallersHandler(cg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"token": "NeverReferenced"}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Equal(t, "[]", resultText(t, res),
		"empty-result path keeps the bare-array response (backward-compat)")
}

// TestCompositeGraph_MountPrefixOf is the unit test for the public
// accessor added in this PR. Empty / virtual-root / unknown-prefix
// inputs all return "" so handlers can distinguish "this id is
// composite-routed" from "this id was never under any mount."
func TestCompositeGraph_MountPrefixOf(t *testing.T) {
	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", graph.NewMemoryStore()))

	cases := []struct {
		name string
		id   string
		want string
	}{
		{"empty_id", "", ""},
		{"unknown_prefix", "billing/foo", ""},
		{"known_prefix_root", "auth", "auth"},
		{"known_prefix_path", "auth/functions/Foo", "auth"},
		{"leading_slash_stripped", "/auth/functions/Foo", "auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cg.MountPrefixOf(tc.id))
		})
	}
}
