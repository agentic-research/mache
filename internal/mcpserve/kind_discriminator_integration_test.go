package mcpserve

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/require"
)

// buildAmbiguousNameFixture seeds a MemoryStore with a synthetic
// project that contains the SAME name `Encoder` in TWO construct
// kinds — a function and a type. This is the failure mode the kind
// discriminator exists to disambiguate. The fixture also stages
// callers in two construct kinds (a function-caller and a method-
// caller of `Helper`) so we can exercise find_callers with kind.
//
// Layout:
//
//	pkg/
//	├── functions/
//	│   ├── Encoder/source       — function named Encoder
//	│   └── DecodeAll/source     — function-caller of Helper
//	├── types/
//	│   └── Encoder/source       — type named Encoder (same name!)
//	├── methods/
//	│   └── Wrapper.Encode/source — method-caller of Helper
//	└── functions/
//	    └── Helper/source        — the called function
func buildAmbiguousNameFixture(t *testing.T) *graph.MemoryStore {
	t.Helper()
	store := graph.NewMemoryStore()

	// Roots + leaf nodes. The Mode/Children fields are required so
	// GetCallers can return *Node entries (it looks nodes up by ID).
	mkLeaf := func(id string) {
		store.AddRoot(&graph.Node{ID: id, Mode: fs.ModeDir, Children: []string{}})
	}
	mkLeaf("pkg/functions/Encoder/source")
	mkLeaf("pkg/functions/DecodeAll/source")
	mkLeaf("pkg/functions/Helper/source")
	mkLeaf("pkg/types/Encoder/source")
	mkLeaf("pkg/methods/Wrapper.Encode/source")

	// Defs: `Encoder` exists as BOTH a function and a type.
	require.NoError(t, store.AddDef("Encoder", "pkg/functions/Encoder/source"))
	require.NoError(t, store.AddDef("Encoder", "pkg/types/Encoder/source"))
	require.NoError(t, store.AddDef("Helper", "pkg/functions/Helper/source"))

	// Refs: `Helper` is called by a FUNCTION (DecodeAll) AND a
	// METHOD (Wrapper.Encode). The construct paths encode the
	// kind of the caller, which is what the kind filter narrows on.
	require.NoError(t, store.AddRef("Helper", "pkg/functions/DecodeAll/source"))
	require.NoError(t, store.AddRef("Helper", "pkg/methods/Wrapper.Encode/source"))

	return store
}

// extractDirIDs pulls the definition paths out of a find_definition
// JSON response. Returns the raw string slice so individual tests
// can assert membership without re-parsing.
func extractDirIDs(t *testing.T, raw string) []string {
	t.Helper()
	var parsed struct {
		Symbol      string   `json:"symbol"`
		Definitions []string `json:"definitions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal find_definition response %q: %v", raw, err)
	}
	return parsed.Definitions
}

// TestKindDiscriminator_AmbiguousName_FindDefinition is the
// load-bearing proof of the kind work. It sets up the exact
// failure mode the discriminator exists for — `Encoder` as both a
// function and a type — and asserts that:
//
//  1. Without kind, find_definition returns BOTH definitions.
//  2. With kind="function", returns ONLY the function definition.
//  3. With kind="type", returns ONLY the type definition.
//  4. With kind="method" (legal kind, but no matching def), returns
//     a "no definition found" hint that NAMES the kind filter so
//     agents can distinguish "wrong kind" from "wrong symbol".
//  5. With kind="wibble" (unknown), returns a structured error
//     listing the accepted kinds (cclsp "fail loud" pattern).
//
// Falsifies: any future change to the filter that breaks
// disambiguation on real ambiguous fixtures, regardless of whether
// the unit tests still pass.
func TestKindDiscriminator_AmbiguousName_FindDefinition(t *testing.T) {
	store := buildAmbiguousNameFixture(t)
	handler := makeFindDefinitionHandler(store)

	t.Run("no_kind_returns_both", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"symbol": "Encoder"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, testutil.ResultText(t, result))
		require.Len(t, dirs, 2, "without kind, both definitions should return; got %v", dirs)
		require.Contains(t, dirs, "pkg/functions/Encoder/source")
		require.Contains(t, dirs, "pkg/types/Encoder/source")
	})

	t.Run("kind_function_narrows_to_function", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"symbol": "Encoder", "kind": "function"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, testutil.ResultText(t, result))
		require.Equal(t, []string{"pkg/functions/Encoder/source"}, dirs,
			"kind=function should narrow to the function-Encoder ONLY")
	})

	t.Run("kind_type_narrows_to_type", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"symbol": "Encoder", "kind": "type"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		dirs := extractDirIDs(t, testutil.ResultText(t, result))
		require.Equal(t, []string{"pkg/types/Encoder/source"}, dirs,
			"kind=type should narrow to the type-Encoder ONLY")
	})

	t.Run("kind_method_returns_no_match_with_kind_hint", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"symbol": "Encoder", "kind": "method"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		text := testutil.ResultText(t, result)
		require.Contains(t, text, `no definition found for "Encoder"`,
			"response should name the symbol")
		require.Contains(t, text, "kind=method",
			"response MUST name the kind filter so agents can distinguish 'wrong kind' from 'wrong symbol'; got %q", text)
	})

	t.Run("kind_unknown_returns_structured_error", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"symbol": "Encoder", "kind": "wibble"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		text := testutil.ResultText(t, result)
		require.Contains(t, text, "unknown kind",
			"response should name the failure mode; got %q", text)
		// Error should enumerate the accepted set — match against a
		// known-valid kind to prove enumeration happens.
		require.Contains(t, text, "function",
			"error should list accepted kinds (cclsp 'fail loud' pattern); got %q", text)
	})
}

// TestKindDiscriminator_AmbiguousName_FindCallers exercises the
// other end of the kind filter: narrowing the CALLER side. Helper
// is called by both a function-caller and a method-caller; kind
// narrows which kinds of callers are returned.
func TestKindDiscriminator_AmbiguousName_FindCallers(t *testing.T) {
	store := buildAmbiguousNameFixture(t)
	handler := makeFindCallersHandler(store)

	extractCallers := func(t *testing.T, raw string) []string {
		t.Helper()
		// find_callers may return either a bare []string OR a
		// {callers: [...]} structure depending on backend / LSP
		// availability. Try the bare slice first.
		var bare []string
		if err := json.Unmarshal([]byte(raw), &bare); err == nil {
			return bare
		}
		var wrapped struct {
			Callers []string `json:"callers"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapped); err != nil {
			t.Fatalf("unmarshal find_callers response %q: %v", raw, err)
		}
		return wrapped.Callers
	}

	t.Run("no_kind_returns_both", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"token": "Helper"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		callers := extractCallers(t, testutil.ResultText(t, result))
		require.Len(t, callers, 2)
		require.Contains(t, callers, "pkg/functions/DecodeAll/source")
		require.Contains(t, callers, "pkg/methods/Wrapper.Encode/source")
	})

	t.Run("kind_function_narrows_to_function_caller", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"token": "Helper", "kind": "function"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		callers := extractCallers(t, testutil.ResultText(t, result))
		require.Equal(t, []string{"pkg/functions/DecodeAll/source"}, callers,
			"kind=function should keep ONLY function-callers")
	})

	t.Run("kind_method_narrows_to_method_caller", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"token": "Helper", "kind": "method"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		callers := extractCallers(t, testutil.ResultText(t, result))
		require.Equal(t, []string{"pkg/methods/Wrapper.Encode/source"}, callers,
			"kind=method should keep ONLY method-callers")
	})

	t.Run("kind_unknown_returns_structured_error", func(t *testing.T) {
		req := testutil.MakeRequest(map[string]any{"token": "Helper", "kind": "wibble"})
		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		require.True(t, strings.Contains(testutil.ResultText(t, result), "unknown kind"),
			"unknown kind should fail loud")
	})
}
