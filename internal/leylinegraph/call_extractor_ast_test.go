package leylinegraph

import (
	"database/sql"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedASTCallFixture builds a ley-line-shaped db holding one Go call:
// `Bar()` in main.go.
//
// AST shape (matches Go's bare call pattern: OuterKind=call_expression,
// LeafKind=identifier):
//
//	main.go
//	└── call_expression  (outer)
//	    ├── identifier     (leaf, record="Bar")
//	    └── argument_list
func seedASTCallFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, 6))
	b.ASTNode("main.go/call_expression", "call_expression", "main.go", fixturedb.Bytes(0, 5))
	b.ASTNode("main.go/call_expression/identifier", "identifier", "main.go", fixturedb.Bytes(0, 3), fixturedb.Detail{Token: "Bar", Field: "function"})
	b.ASTNode("main.go/call_expression/argument_list", "argument_list", "main.go", fixturedb.Bytes(3, 5), fixturedb.Detail{Field: "arguments"})
	_, f := b.Build()
	return f.DB(), "main.go"
}

// seedNoASTFixture builds a db with the mache projection's own schema and no
// `_ast` table — what a non-source backend hands the pickers.
func seedNoASTFixture(t *testing.T) *sql.DB {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Standalone)
	b.Def("Bar", "pkg/functions/Bar", fixturedb.Function)
	_, f := b.Build()
	return f.DB()
}

// TestNewASTCallExtractor_ResolvesGoCall pins the basic happy path:
// given a synthetic _ast row for a Go call_expression, the extractor
// returns the call token. Mirrors what newCallExtractor (CGO) returns
// for the same input shape, but via SQL — no tree-sitter, no parser.
func TestNewASTCallExtractor_ResolvesGoCall(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, sourcePath, "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "synthetic call_expression(identifier=Bar) must surface")
	assert.Equal(t, "Bar", calls[0].Token)
	assert.Empty(t, calls[0].Qualifier, "bare identifier pattern has no qualifier")
}

// TestNewASTCallExtractor_UnknownLanguageReturnsNil mirrors the
// SitterWalker-backed extractor's "no grammar" behavior — when the
// language has no registered call pattern, return nil, nil rather
// than erroring. Callers treat empty as "no calls in this file."
func TestNewASTCallExtractor_UnknownLanguageReturnsNil(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, sourcePath, "esperanto")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestASTCallExtractor_ContentArgIgnored pins the contract that the AST
// extractor's `content` parameter is unused — calls are resolved from the
// pre-parsed _ast table keyed by `path`, not by re-parsing. That is the
// central design difference from the CGO extractor it replaced, and it is
// also how PickCallExtractor's dispatch is observed: CallExtractor is an
// opaque closure, so the only evidence that a db carrying `_ast` got the AST
// extractor is that garbage content still yields the AST-derived call.
func TestASTCallExtractor_ContentArgIgnored(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*sql.DB) graph.CallExtractor
	}{
		{"NewASTCallExtractor", NewASTCallExtractor},
		{"PickCallExtractor", PickCallExtractor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, sourcePath := seedASTCallFixture(t)

			extract := tc.build(db)
			calls, err := extract([]byte("not Go at all — totally bogus bytes"), sourcePath, "go")
			require.NoError(t, err)
			require.Len(t, calls, 1, "extractor must trust the AST, not the content arg")
			assert.Equal(t, "Bar", calls[0].Token)
		})
	}
}

// TestNewASTCallExtractor_NonexistentSourcePathReturnsEmpty pins
// graceful handling of a stale path arg — querying _ast for a
// source_id that doesn't exist yields no rows, not an error. Same
// shape as the SitterWalker-backed extractor's response when given
// an empty content slice.
func TestNewASTCallExtractor_NonexistentSourcePathReturnsEmpty(t *testing.T) {
	db, _ := seedASTCallFixture(t)

	extract := NewASTCallExtractor(db)
	calls, err := extract(nil, "does/not/exist.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestPickCallExtractor_FallsBackWhenASTAbsent pins the inverse: a db
// with no `_ast` table gets the no-op extractor — there is no CGO fallback
// since ADR-0012 step 4 — which resolves nothing and never errors.
func TestPickCallExtractor_FallsBackWhenASTAbsent(t *testing.T) {
	extract := PickCallExtractor(seedNoASTFixture(t))
	require.NotNil(t, extract, "fallback extractor must not be nil")
	calls, err := extract([]byte("package main\n\nfunc A() { Bar() }\n"), "main.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls, "without _ast there is nothing to resolve calls from")
}

// TestPickCallExtractor_HandlesNilDB pins the safety contract for
// callers that might pass a nil DB handle (not a current call site
// but a contract worth preserving as wiring evolves).
func TestPickCallExtractor_HandlesNilDB(t *testing.T) {
	extract := PickCallExtractor(nil)
	require.NotNil(t, extract, "nil DB must yield the no-op extractor, not a nil closure")
	calls, err := extract(nil, "main.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestNewASTScopedCallExtractor_ResolvesGoCall pins the scoped-extractor
// wiring (bead mache-fd9982): unlike NewASTCallExtractor, sourceID/scopeID
// here are the REAL `_ast` source_id + scope node id, not a graph node id.
// With an empty scopeID (whole-file match, mirroring the unscoped fixture),
// it must still resolve the same call NewASTCallExtractor finds.
func TestNewASTScopedCallExtractor_ResolvesGoCall(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)

	extract := NewASTScopedCallExtractor(db)
	calls, err := extract(sourcePath, "", "go")
	require.NoError(t, err)
	require.Len(t, calls, 1, "synthetic call_expression(identifier=Bar) must surface")
	assert.Equal(t, "Bar", calls[0].Token)
	assert.Empty(t, calls[0].Qualifier, "bare identifier pattern has no qualifier")
}

// TestNewASTScopedCallExtractor_NonexistentScopeReturnsEmpty pins the
// scoping contract: a scopeID prefix that matches nothing in `_ast` yields
// no calls, not an error — the same "stale/mismatched id degrades to empty"
// shape as the unscoped extractor's nonexistent-source-path test.
func TestNewASTScopedCallExtractor_NonexistentScopeReturnsEmpty(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)

	extract := NewASTScopedCallExtractor(db)
	calls, err := extract(sourcePath, "no/such/scope", "go")
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestPickScopedCallExtractor_PrefersASTWhenAvailable mirrors
// TestPickCallExtractor_PrefersASTWhenAvailable for the scoped picker: a
// .db carrying `_ast` yields a working scoped extractor.
func TestPickScopedCallExtractor_PrefersASTWhenAvailable(t *testing.T) {
	db, sourcePath := seedASTCallFixture(t)

	extract := PickScopedCallExtractor(db)
	require.NotNil(t, extract)
	calls, err := extract(sourcePath, "", "go")
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "Bar", calls[0].Token)
}

// TestPickScopedCallExtractor_NilWhenASTAbsentOrDBNil pins the inverse of
// the CallExtractor picker: since there is no CGO fallback for the scoped
// extractor, absence of `_ast` (or a nil db) must yield nil, not a closure
// that would silently no-op. Callers (GetCallees) already treat a nil
// scopedExtractor as "fall back to the legacy path".
func TestPickScopedCallExtractor_NilWhenASTAbsentOrDBNil(t *testing.T) {
	assert.Nil(t, PickScopedCallExtractor(nil))
	assert.Nil(t, PickScopedCallExtractor(seedNoASTFixture(t)))
}
