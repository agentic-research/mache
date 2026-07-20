package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The whole-file extraction caches (fileCallTokens / fileAddrRefs) must NOT
// store an empty partial result on a DB failure — they must surface the error.
// Otherwise a transient DB error on a long-lived serve would permanently empty
// a file's callees/refs with zero signal (mache-015f5c). "go" call patterns and
// address-ref queries are registered in engine_languages' init, so a closed DB
// drives the real query path to failure.

func TestExtractCallsScoped_SurfacesDBError(t *testing.T) {
	db := seedTestAST(t)
	w := NewASTWalker(db)
	require.NoError(t, db.Close()) // subsequent queries fail

	_, err := w.ExtractCallsScoped("main.go", "main.go/scope", "go")
	require.Error(t, err, "a DB failure must surface, not be cached as an empty callee set")
}

func TestExtractAddressRefs_SurfacesDBError(t *testing.T) {
	db := seedTestAST(t)
	w := NewASTWalker(db)
	require.NoError(t, db.Close())

	_, err := w.ExtractAddressRefs("main.go", "go")
	require.Error(t, err, "a DB failure must surface, not be cached as an empty ref set")
}
