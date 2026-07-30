package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestDocRefsView_ScopesToRustAndStripsCallSyntax pins the view's shape
// directly, independent of the drift_doc_dead_symbol_reference rule that
// consumes it: Rust-path tokens survive, non-Rust and malformed ones are
// filtered before any consumer sees them, and a trailing '()' is stripped
// into a separate bare column without mutating the original token.
func TestDocRefsView_ScopesToRustAndStripsCallSyntax(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "docrefs.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE node_refs (token TEXT, node_id TEXT, source_id TEXT, container_node_id TEXT, qualifier TEXT);
		INSERT INTO node_refs (token, node_id, source_id) VALUES
		  ('epic::is_dominated_by', 'doc.md/span0', 'doc.md'),
		  ('Stdio::null()', 'doc.md/span1', 'doc.md'),
		  ('MaxRetries', 'doc.md/span2', 'doc.md'),
		  ('cmd/serve.go::registerMCPTools', 'doc.md/span3', 'doc.md'),
		  ('Foo:: bar', 'doc.md/span4', 'doc.md'),
		  ('epic::is_dominated_by', 'main.go/call0', 'main.go');
	`)
	require.NoError(t, err)

	_, err = db.Exec("CREATE TEMP TABLE v_doc_refs AS " + docRefsViewSQL(true))
	require.NoError(t, err)

	rows, err := db.Query(`SELECT token, node_id, source_id, bare FROM v_doc_refs ORDER BY node_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type docRefRow struct{ token, nodeID, sourceID, bare string }
	var got []docRefRow
	for rows.Next() {
		var r docRefRow
		require.NoError(t, rows.Scan(&r.token, &r.nodeID, &r.sourceID, &r.bare))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 2, "MaxRetries (no ::), the file.go::Symbol path shape, and the whitespace-containing "+
		"span must all be excluded; the non-.md source row must be excluded")
	assert.Equal(t, "epic::is_dominated_by", got[0].token)
	assert.Equal(t, "epic::is_dominated_by", got[0].bare, "no trailing () — bare equals token")
	assert.Equal(t, "Stdio::null()", got[1].token, "original token is untouched")
	assert.Equal(t, "Stdio::null", got[1].bare, "bare strips the trailing call syntax")
}

// TestDocRefsView_DegradesToEmptyWithoutSourceID is the counterpart to
// smell_test_nodes.go's own AST-absence test: a node_refs table without a
// source_id column (mache's schema-projection output, and most hand-built
// test fixtures) must yield zero rows rather than a "no such column" error
// — that's the entire reason this is a Go-probed view and not an inline
// query in the consuming rule.
func TestDocRefsView_DegradesToEmptyWithoutSourceID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nosourceid.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE node_refs (token TEXT, node_id TEXT)`)
	require.NoError(t, err)

	_, err = db.Exec("CREATE TEMP TABLE v_doc_refs AS " + docRefsViewSQL(false))
	require.NoError(t, err, "the no-source_id form must be valid SQL against a node_refs missing that column")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_doc_refs`).Scan(&n))
	assert.Zero(t, n)
}
