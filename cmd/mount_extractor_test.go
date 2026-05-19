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

// TestNewCallExtractor_EmptyGoContent — exercises the parse-success path
// with valid Go that produces a call. The NotNil assertion is what
// distinguishes this from the unknown-language short-circuit (which
// returns nil, nil). Without NotNil the test would pass for both paths
// because require.Empty accepts nil; with NotNil the test enforces that
// the grammar resolved and the parser actually ran.
func TestNewCallExtractor_EmptyGoContent(t *testing.T) {
	extract := newCallExtractor()
	calls, err := extract([]byte("package p\nfunc f() { g() }\n"), "/tmp/empty.go", "go")
	require.NoError(t, err)
	require.NotNil(t, calls, "Go grammar registered: parse must return non-nil slice")
	require.NotEmpty(t, calls, "parsed Go source with a call must yield at least one QualifiedCall")
}
