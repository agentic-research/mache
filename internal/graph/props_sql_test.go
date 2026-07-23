package graph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"

	_ "modernc.org/sqlite"
)

// machePropsDB builds a mache-shaped nodes table (context + props) holding one
// construct that carries lang and imports.
func machePropsDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "serve.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL,
			record_id TEXT, record JSON, source_file TEXT, context BLOB, props JSON
		);
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		INSERT INTO nodes VALUES ('main', '', 'main', 1, 0, 1, NULL, NULL, NULL, NULL,
			'{"lang":"go","pkg":"main","imports":{"fmt":"fmt","http":"net/http"}}');
		INSERT INTO nodes VALUES ('other', '', 'other', 1, 0, 1, NULL, NULL, NULL, NULL,
			'{"lang":"python"}');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return dbPath
}

// TestServedNodeKeepsPropertiesAndSQLCanQueryThem closes the loop mache-90b89b
// was filed for. The Go-side assertion alone would pass even if the stored form
// were still base64 — it only proves the encoder and decoder agree with each
// other. The SQL assertion is the one that pins the actual goal.
func TestServedNodeKeepsPropertiesAndSQLCanQueryThem(t *testing.T) {
	dbPath := machePropsDB(t)

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	node, err := g.GetNode("main")
	require.NoError(t, err)
	assert.Equal(t, "go", PropString(node, "lang"),
		"a construct read from .db must keep lang (the v0.19.0 serve-time fix)")
	assert.Equal(t, "main", PropString(node, "pkg"))
	assert.JSONEq(t, `{"fmt":"fmt","http":"net/http"}`, string(PropRaw(node, "imports")),
		"imports must arrive as a real object, not a base64 blob")

	// The point of the change: SQL can filter on these.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM nodes WHERE json_extract(props,'$.lang')='go'`).Scan(&n))
	assert.Equal(t, 1, n,
		"SQL must filter nodes by lang — this is what mache-90b89b unblocks for smell rules")

	var imp string
	require.NoError(t, db.QueryRow(
		`SELECT json_extract(props,'$.imports.fmt') FROM nodes WHERE id='main'`).Scan(&imp))
	assert.Equal(t, "fmt", imp, "nested object fields must be reachable, not just top-level keys")
}

// TestStaleMacheDBIsRefusedOnOpen pins the cutover at the boundary users hit.
func TestStaleMacheDBIsRefusedOnOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER,
		size INTEGER, mtime INTEGER, record_id TEXT, record JSON,
		source_file TEXT, context BLOB
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStalePropsSchema)
	assert.Contains(t, err.Error(), "mache build")
}

// TestLeylineShapedDBOpensFine is the regression guard for the producer
// scoping. leyline parse has been mache's sole source parser since v0.18.0, so
// refusing its output would break the primary path, not a legacy corner.
func TestLeylineShapedDBOpensFine(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "leyline.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER,
		size INTEGER, mtime INTEGER, record_id TEXT, record JSON, source_file TEXT
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err, "a leyline-shaped nodes table must open, not be refused")
	t.Cleanup(func() { _ = g.Close() })
}
