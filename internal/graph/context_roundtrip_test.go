package graph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestNodesTableReader_GetNode_CarriesContext pins bead mache-b8fe72: the
// mount reads nodes via NodesTableReader.GetNode, but it never selected the
// `context` column, so node.Context was always nil and the headline
// `cat context` virtual file (served by vfs.ContextHandler when
// len(node.Context) > 0) was absent for every construct. node.Context is
// populated at ingest (engine_walk.go) and survives in MemoryStore; it was
// only lost on the SQLite persist/read path.
func TestNodesTableReader_GetNode_CarriesContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ctx.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER,
		size INTEGER, mtime INTEGER, record_id TEXT, record JSON,
		source_file TEXT, context BLOB)`)
	require.NoError(t, err)

	ctx := []byte("import (\n\t\"fmt\"\n)\n")
	_, err = db.Exec(`INSERT INTO nodes (id, name, kind, size, mtime, context) VALUES (?, ?, ?, ?, ?, ?)`,
		"pkg/methods/Foo.Bar", "Foo.Bar", NodeKindDir, int64(0), int64(0), ctx)
	require.NoError(t, err)

	r := NewNodesTableReader(db, "results", stubRender, nil, 0o444, 0o555, 16)
	got, err := r.GetNode("pkg/methods/Foo.Bar")
	require.NoError(t, err)
	assert.Equal(t, ctx, got.Context,
		"mount path (NodesTableReader.GetNode) must carry node.Context — mache-b8fe72")
}

// TestNodesTableReader_GetNode_MissingContextColumn is the backward-compat
// guard: older mache builds and leyline-produced nodes tables predate the
// `context` column. GetNode must degrade to an empty Context, never error.
func TestNodesTableReader_GetNode_MissingContextColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER,
		size INTEGER, mtime INTEGER, record_id TEXT, record JSON,
		source_file TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes (id, name, kind, size, mtime) VALUES (?, ?, ?, ?, ?)`,
		"pkg/methods/Foo.Bar", "Foo.Bar", NodeKindDir, int64(0), int64(0))
	require.NoError(t, err)

	r := NewNodesTableReader(db, "results", stubRender, nil, 0o444, 0o555, 16)
	got, err := r.GetNode("pkg/methods/Foo.Bar")
	require.NoError(t, err, "GetNode on a pre-context-column nodes table must not error")
	assert.Empty(t, got.Context)
}
