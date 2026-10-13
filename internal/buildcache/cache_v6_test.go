package buildcache

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// A push from a projection-v6 .db (leyline v0.20.x) fails, naming why: a
// chunk carries _ast in projection-v4's shape, and carrying v6's would mean
// mache writing more of LLO's tables, which mache-e64f36 replaces. It must not
// fall back to source-only chunks, which would restore a database without its
// AST and say nothing.
func TestCachePush_RefusesProjectionV6(t *testing.T) {
	b := fixturedb.New(t, fixturedb.LeylineV6)
	b.Source("a.go", "go", "package a\n")
	b.ASTNode("a.go", "source_file", "a.go", fixturedb.Bytes(0, 10))
	dbPath, _ := b.Build()

	err := runCachePush(new(bytes.Buffer), dbPath, t.TempDir())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProjectionV6), "got %v", err)
	assert.Contains(t, err.Error(), "mache-e64f36")
}

// The same spec written as projection-v4 still pushes, with its AST.
func TestCachePush_ProjectionV4StillPushes(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	b.ASTNode("a.go", "source_file", "a.go", fixturedb.Bytes(0, 10))
	dbPath, _ := b.Build()
	require.NoError(t, runCachePush(new(bytes.Buffer), dbPath, t.TempDir()))
}
