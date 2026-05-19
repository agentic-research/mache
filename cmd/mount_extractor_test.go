package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewCallExtractor_UnknownLanguage verifies that an unsupported language
// short-circuits to (nil, nil) — the contract callers depend on for
// languages outside the tree-sitter registry (e.g. plain text, binary).
func TestNewCallExtractor_UnknownLanguage(t *testing.T) {
	extract := newCallExtractor()
	calls, err := extract([]byte("anything"), "/tmp/file", "no-such-language")
	require.NoError(t, err)
	require.Nil(t, calls)
}

// TestNewCallExtractor_NilTreeForUnsupportedContent — empty content with a
// real language still produces a tree (tree-sitter parses to an empty
// root), so the explicit `tree == nil` branch is defensive against
// future tree-sitter API changes. We exercise the success-with-empty
// path here to lock in current behavior.
func TestNewCallExtractor_EmptyGoContent(t *testing.T) {
	extract := newCallExtractor()
	calls, err := extract([]byte(""), "/tmp/empty.go", "go")
	require.NoError(t, err)
	// Empty source produces no calls — but the parse path must complete
	// without error so the extractor returns an empty slice (not nil
	// from the short-circuit branch).
	require.Empty(t, calls)
}
