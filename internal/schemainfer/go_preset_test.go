package schemainfer

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenCorpus is the hand-written Go corpus the golden projection pins. It is
// the go preset's fixture for the same reason preset_fixtures/ serves the
// others: every construct in it exercises one projection path.
func goldenCorpus(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return filepath.Join(root, "testdata", "snapshots", "small-go-golden")
}

// TestGoPreset_GenericReceiversAreMethods pins mache-51571b: a method on a
// generic receiver is a method.
//
// Before the fix the preset's two receiver shapes stopped at
// `(pointer_type (type_identifier))` and `(type_identifier)`, but a generic
// receiver parses as pointer_type → generic_type → type_identifier (or bare
// generic_type → type_identifier). Neither shape reached it, so every method
// on a generic type was projected NOWHERE — not under methods/, not under
// functions/, with no routing warning and no diagnostic. The constructs simply
// vanished, which is why dead_code, find_callers and untested_function could
// never see them.
//
// The receiver is the generic type's NAME, not its instantiation: Stack, not
// Stack[T] — consistent with the rust preset's Cell.new (mache-c777ef).
func TestGoPreset_GenericReceiversAreMethods(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, goldenCorpus(t))
	store := projectPreset(t, "go", astDB, parseDir)

	methods := projectedNames(t, store, "main/methods")
	assert.Contains(t, methods, "Stack.Push", "pointer receiver on a generic type")
	assert.Contains(t, methods, "Stack.Len", "value receiver on a generic type")
	assert.Contains(t, methods, "Pair.Swap", "receiver with two type parameters")
	for _, m := range methods {
		assert.NotContains(t, m, "[", "%s: type arguments leaked into the receiver name", m)
	}

	// The non-generic shapes still work — the generic selectors are listed
	// first and sibling schema nodes are an ordered choice (mache-c777ef), so
	// a too-greedy generic shape would silently claim these instead.
	store2 := projectPreset(t, "go", astDB, parseDir)
	storeMethods := projectedNames(t, store2, "store/methods")
	assert.Contains(t, storeMethods, "Store.Add", "pointer receiver, non-generic")
	assert.Contains(t, storeMethods, "Store.Len", "value receiver, non-generic")

	// A method is never also a free function.
	for _, fn := range projectedNames(t, store, "main/functions") {
		assert.NotContains(t, []string{"Push", "Len", "Swap"}, fn,
			"%s is a method; it must not appear under functions/", fn)
	}
}

// TestGoPreset_EveryMethodDeclarationProjectedOnce is the corpus-scale form of
// the same invariant, and the one that fails if a NEW receiver shape appears
// that no selector covers: every `method_declaration` ley-line parsed is
// projected exactly once under some methods/ directory.
func TestGoPreset_EveryMethodDeclarationProjectedOnce(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, goldenCorpus(t))
	store := projectPreset(t, "go", astDB, parseDir)

	var declared int
	require.NoError(t, astDB.QueryRow(
		`SELECT count(*) FROM _ast WHERE node_kind = 'method_declaration'`).Scan(&declared))
	require.Positive(t, declared)

	projected := 0
	for _, pkg := range []string{"main", "store"} {
		projected += len(projectedNames(t, store, pkg+"/methods"))
	}
	t.Logf("%d method_declaration nodes; %d projected under methods/", declared, projected)
	assert.Equal(t, declared, projected,
		"%d method_declaration nodes parsed but %d reached methods/; a receiver shape "+
			"has no selector and its methods are projected nowhere", declared, projected)
}
