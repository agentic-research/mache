package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// An owned graph keeps SQLite's shared lock from its first read until it
// closes, instead of taking and releasing it on every statement: that per-
// statement fcntl churn was two thirds of get_architecture's CPU on mache's
// own repo (mache-86c666). The lock being held is observable from outside: a
// writer cannot commit while the graph is open, and can once it closes.
func TestOpenOwned_HoldsTheReadLockForTheGraphsLife(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("Only", "pkg/file.go/function_declaration_0", "function")
	path, _ := b.Build()

	g, err := OpenOwnedSQLiteGraph(path, &api.Topology{}, machetmpl.Render)
	require.NoError(t, err)
	require.NotEmpty(t, g.LookupDef("Only"), "the owned graph must still answer")

	assert.Error(t, fixturedb.TryCommit(path),
		"a writer committed while the owned graph was open: its connection released the shared lock after the read")

	require.NoError(t, g.Close())
	assert.NoError(t, fixturedb.TryCommit(path), "closing the graph must release the lock")
}

// The default opener is for a db something else may write, so it must not
// hold the lock between statements.
func TestOpen_ReleasesTheReadLockBetweenStatements(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("Only", "pkg/file.go/function_declaration_0", "function")
	path, _ := b.Build()

	g, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	require.NotEmpty(t, g.LookupDef("Only"))

	assert.NoError(t, fixturedb.TryCommit(path), "the default opener held the read lock after a read")
}
