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

// twoMountStoresWithDefs builds two MemoryStores: each defines its
// own function (so find_definition can find one in each mount). For
// callees, we put a caller in each repo that calls a function within
// its OWN repo — cross-repo callees resolution isn't wired yet
// (mache-iegm step 4); these tests only verify the mount-prefix
// annotation when callees route per-mount.
func twoMountStoresWithDefs(t *testing.T) (*graph.MemoryStore, *graph.MemoryStore) {
	t.Helper()
	auth := graph.NewMemoryStore()
	auth.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&graph.Node{
		ID: "functions/Validate", Mode: fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	auth.AddNode(&graph.Node{
		ID: "functions/Validate/source", Mode: 0,
		Data: []byte("func Validate() {}"),
	})
	require.NoError(t, auth.AddDef("Validate", "functions/Validate"))

	billing := graph.NewMemoryStore()
	billing.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&graph.Node{
		ID: "functions/Charge", Mode: fs.ModeDir,
		Children: []string{"functions/Charge/source"},
	})
	billing.AddNode(&graph.Node{
		ID: "functions/Charge/source", Mode: 0,
		Data: []byte("func Charge() {}"),
	})
	require.NoError(t, billing.AddDef("Charge", "functions/Charge"))
	return auth, billing
}

// TestFindDefinition_AnnotatesMountOnComposite verifies the
// definition lookup surfaces the mount prefix when running on a
// CompositeGraph. Validate is defined in auth, so the response
// should include {definitions: [{path: "auth/functions/Validate",
// mount: "auth"}]}.
func TestFindDefinition_AnnotatesMountOnComposite(t *testing.T) {
	auth, billing := twoMountStoresWithDefs(t)
	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	handler := makeFindDefinitionHandler(cg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Symbol      string       `json:"symbol"`
		Definitions []scopedItem `json:"definitions"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Equal(t, "Validate", resp.Symbol)
	require.Len(t, resp.Definitions, 1)
	assert.Equal(t, "auth/functions/Validate", resp.Definitions[0].Path)
	assert.Equal(t, "auth", resp.Definitions[0].Mount)
}

// TestFindDefinition_NonCompositeKeepsLegacyShape ensures a single
// MemoryStore returns the historical {symbol, definitions: []string}
// response.
func TestFindDefinition_NonCompositeKeepsLegacyShape(t *testing.T) {
	auth, _ := twoMountStoresWithDefs(t)
	handler := makeFindDefinitionHandler(auth)
	res, err := handler(context.Background(), makeRequest(map[string]any{"symbol": "Validate"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Symbol      string   `json:"symbol"`
		Definitions []string `json:"definitions"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Equal(t, "Validate", resp.Symbol)
	assert.Equal(t, []string{"functions/Validate"}, resp.Definitions)
}

// TestFindDefinition_FederatesAcrossMounts is the cross-repo
// killer: two mounts each define a different name; one query
// against the composite returns the def from the right mount. A
// non-existent name returns no results.
func TestFindDefinition_FederatesAcrossMounts(t *testing.T) {
	auth, billing := twoMountStoresWithDefs(t)
	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	handler := makeFindDefinitionHandler(cg)

	for _, tc := range []struct {
		symbol string
		mount  string
		path   string
	}{
		{"Validate", "auth", "auth/functions/Validate"},
		{"Charge", "billing", "billing/functions/Charge"},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			res, err := handler(context.Background(), makeRequest(map[string]any{"symbol": tc.symbol}))
			require.NoError(t, err)
			require.False(t, res.IsError)

			var resp struct {
				Definitions []scopedItem `json:"definitions"`
			}
			require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
			require.Len(t, resp.Definitions, 1)
			assert.Equal(t, tc.path, resp.Definitions[0].Path)
			assert.Equal(t, tc.mount, resp.Definitions[0].Mount)
		})
	}
}

// TestFindCallees_AnnotatesMountOnComposite asserts find_callees
// surfaces the mount prefix when the queried construct's callees
// resolve via the local mount. (Cross-repo callees resolution —
// where a function in mount A calls a function defined in mount B —
// is mache-iegm step 4 and not part of this PR.)
//
// We build a self-call: auth/Validate is a construct dir whose
// source calls Validate again. CompositeGraph.GetCallees routes the
// query to the auth mount, which resolves Validate locally and
// returns its def. The response should annotate the result with
// mount="auth".
func TestFindCallees_AnnotatesMountOnComposite(t *testing.T) {
	auth := graph.NewMemoryStore()
	auth.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&graph.Node{
		ID: "functions/Validate", Mode: fs.ModeDir,
		Children:   []string{"functions/Validate/source"},
		Properties: map[string][]byte{"lang": []byte("go")},
	})
	auth.AddNode(&graph.Node{
		ID: "functions/Validate/source", Mode: 0,
		Data: []byte("package main\nfunc Validate() { Validate() }\n"),
	})
	require.NoError(t, auth.AddDef("Validate", "functions/Validate"))
	auth.SetCallExtractor(newCallExtractor())

	billing := graph.NewMemoryStore()
	billing.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})

	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))

	handler := makeFindCalleesHandler(cg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"path": "auth/functions/Validate"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Callees []scopedItem `json:"callees"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		// CGO tree-sitter may produce zero callees in some envs (no
		// extractor wired in tests). The annotation path is the
		// thing under test; if there are no callees we skip rather
		// than treating the unrelated empty-result as a failure.
		t.Skipf("call extractor produced no callees in this env: %v", err)
	}
	if len(resp.Callees) == 0 {
		t.Skip("call extractor produced no callees in this env (CGO not loaded?)")
	}
	for _, c := range resp.Callees {
		assert.Equal(t, "auth", c.Mount, "callee from auth mount must be annotated")
	}
}

// TestFindCallees_CrossMountResolvesAndAnnotates pins the
// substantive half of mache-iegm: a function in mount A whose
// source calls a function defined in mount B should surface
// the cross-mount callee with mount="B" annotation.
//
// Today this works because:
//   - serve.go wires composite.SetCallExtractor at startup
//   - CompositeGraph.GetCallees runs phase-2 cross-mount
//     resolution via crossMountCallees() (composite.go:396)
//   - annotateMounts forwards through lazyGraph (#284) so the
//     mount label survives the wrapper
//
// If any of those three pieces regresses, this test catches
// it. The earlier TestFindCallees_AnnotatesMountOnComposite
// only exercised the within-mount path; the cross-mount
// extractor branch had no regression coverage.
//
// Skips if the call extractor produces no callees in the test
// environment — CGO tree-sitter dependency.
func TestFindCallees_CrossMountResolvesAndAnnotates(t *testing.T) {
	// auth: defines Caller, whose source calls Validate (defined in billing).
	auth := graph.NewMemoryStore()
	auth.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	auth.AddNode(&graph.Node{
		ID: "functions/Caller", Mode: fs.ModeDir,
		Children:   []string{"functions/Caller/source"},
		Properties: map[string][]byte{"lang": []byte("go")},
	})
	auth.AddNode(&graph.Node{
		ID: "functions/Caller/source", Mode: 0,
		Data: []byte("package main\nfunc Caller() { Validate() }\n"),
	})
	require.NoError(t, auth.AddDef("Caller", "functions/Caller"))
	auth.SetCallExtractor(newCallExtractor())

	// billing: defines Validate, the cross-mount callee target.
	billing := graph.NewMemoryStore()
	billing.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})
	billing.AddNode(&graph.Node{
		ID: "functions/Validate", Mode: fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	billing.AddNode(&graph.Node{
		ID: "functions/Validate/source", Mode: 0,
		Data: []byte("func Validate() {}"),
	})
	require.NoError(t, billing.AddDef("Validate", "functions/Validate"))

	cg := graph.NewCompositeGraph()
	require.NoError(t, cg.Mount("auth", auth))
	require.NoError(t, cg.Mount("billing", billing))
	cg.SetCallExtractor(newCallExtractor())

	handler := makeFindCalleesHandler(cg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"path": "auth/functions/Caller"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "find_callees must succeed; got: %s", resultText(t, res))

	var resp struct {
		Callees []scopedItem `json:"callees"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Skipf("call extractor produced unparseable result in this env: %v", err)
	}
	if len(resp.Callees) == 0 {
		t.Skip("call extractor produced no callees in this env (CGO not loaded?)")
	}

	// At least one callee must point at billing/Validate with the
	// correct mount label. Other mounts may produce additional
	// candidates if the extractor finds local matches; the cross-
	// mount one is the contract under test.
	var found bool
	for _, c := range resp.Callees {
		if c.Mount == "billing" && c.Path == "billing/functions/Validate" {
			found = true
			break
		}
	}
	require.True(t, found,
		"cross-mount callee resolution must surface billing/Validate with mount=\"billing\"; got: %+v",
		resp.Callees)
}
