package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// v_test_nodes detects Rust test code STRUCTURALLY: Rust states test-ness in an
// attribute (`#[cfg(test)] mod tests`, `#[test] fn`) and colocates the result in
// the production file, so no path or name predicate can see it — unlike Go,
// where `_test.go` is a complete answer.
//
// The fixture below encodes the shapes that matter, with byte offsets standing
// in for a real parse:
//
//	prod_fn         a plain function                      -> NOT test
//	#[test]         attribute, then test_fn                  -> test
//	#[cfg(test)]    attribute, then mod tests { inner_fn }   -> BOTH test
//	#[cfg(test)] #[allow] mod stacked { s_fn }               -> BOTH test
//
// Stacking is included because the association is positional: the attribute is
// a SIBLING of what it decorates, not its parent. Taking "the next node of any
// kind" would resolve a stacked `#[cfg(test)] #[allow(...)] mod` to the second
// ATTRIBUTE and silently miss the module — which is why the query filters to
// function_item/mod_item when finding the target.

type testNodeFixtureRow struct {
	nodeID string
	kind   string
	sb, eb int
	token  string // non-empty only for identifier rows
}

// buildTestNodesFixture writes an _ast + node_content pair shaped like leyline
// parse output. Byte ranges are what encode both containment and the
// attribute→item association, so they are chosen to be unambiguous.
func buildTestNodesFixture(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tn.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1) // TEMP tables are per-connection
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL, node_hash BLOB);
		CREATE TABLE node_content (node_hash BLOB PRIMARY KEY, token TEXT);`)
	require.NoError(t, err)

	rows := []testNodeFixtureRow{
		// a production function — must stay unmarked
		{"f/prod_fn", "function_item", 0, 50, ""},

		// #[test] fn test_fn
		{"f/attr1", "attribute_item", 60, 68, ""},
		{"f/attr1/id", "identifier", 62, 66, "test"},
		{"f/test_fn", "function_item", 70, 120, ""},

		// #[cfg(test)] mod tests { inner_fn }
		{"f/attr2", "attribute_item", 130, 143, ""},
		{"f/attr2/cfg", "identifier", 132, 135, "cfg"},
		{"f/attr2/id", "identifier", 136, 140, "test"},
		{"f/mod_tests", "mod_item", 145, 300, ""},
		{"f/mod_tests/inner_fn", "function_item", 180, 250, ""},

		// #[cfg(test)] #[allow(dead_code)] mod stacked { s_fn }
		{"f/attr3", "attribute_item", 310, 323, ""},
		{"f/attr3/cfg", "identifier", 312, 315, "cfg"},
		{"f/attr3/id", "identifier", 316, 320, "test"},
		{"f/attr4", "attribute_item", 325, 348, ""}, // the intervening #[allow(...)]
		{"f/attr4/id", "identifier", 327, 332, "allow"},
		{"f/mod_stacked", "mod_item", 350, 450, ""},
		{"f/mod_stacked/s_fn", "function_item", 380, 430, ""},
	}
	for i, r := range rows {
		var hash any
		if r.token != "" {
			h := []byte{byte(i + 1)}
			hash = h
			_, err = db.Exec(`INSERT OR IGNORE INTO node_content(node_hash, token) VALUES (?, ?)`, h, r.token)
			require.NoError(t, err)
		}
		_, err = db.Exec(`INSERT INTO _ast(node_id, source_id, node_kind, start_byte, end_byte, node_hash)
			VALUES (?, 'f.rs', ?, ?, ?, ?)`, r.nodeID, r.kind, r.sb, r.eb, hash)
		require.NoError(t, err)
	}

	_, err = db.Exec("CREATE TEMP TABLE v_test_nodes AS " + testNodesViewSQL(true))
	require.NoError(t, err)
	return db
}

func markedTestNodes(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT node_id FROM v_test_nodes`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		got[id] = true
	}
	require.NoError(t, rows.Err())
	return got
}

// The core contract: #[test] and #[cfg(test)] both mark, containment is
// transitive, and production code is untouched.
func TestTestNodesView_MarksAttributedAndContained(t *testing.T) {
	got := markedTestNodes(t, buildTestNodesFixture(t))

	assert.True(t, got["f/test_fn"], "#[test] fn must be marked")
	assert.True(t, got["f/mod_tests"], "#[cfg(test)] mod must be marked")
	assert.True(t, got["f/mod_tests/inner_fn"],
		"a fn INSIDE #[cfg(test)] mod must be marked — transitivity is the point, "+
			"otherwise only the mod node is excluded and its whole body is still "+
			"judged as production code")

	assert.False(t, got["f/prod_fn"],
		"a plain production function must NOT be marked; over-marking silently "+
			"disables the rules this feeds")
}

// Stacked attributes. The association is positional and attributes are
// siblings, so a naive "next node" resolves to the second attribute and misses
// the module entirely.
func TestTestNodesView_HandlesStackedAttributes(t *testing.T) {
	got := markedTestNodes(t, buildTestNodesFixture(t))

	assert.True(t, got["f/mod_stacked"],
		"#[cfg(test)] followed by #[allow(...)] before the mod must still resolve "+
			"to the mod — the target search skips intervening attribute_items by "+
			"filtering to function_item/mod_item")
	assert.True(t, got["f/mod_stacked/s_fn"], "and its body with it")
}

// The attribute nodes themselves are not constructs and should not appear as
// findings-suppressing entries in their own right; only what they decorate.
func TestTestNodesView_DoesNotMarkTheAttributeNodes(t *testing.T) {
	got := markedTestNodes(t, buildTestNodesFixture(t))
	for _, id := range []string{"f/attr1", "f/attr2", "f/attr3"} {
		assert.False(t, got[id], "%s is the attribute, not the construct it decorates", id)
	}
}

// A projection without _ast (mache's schema projections, JSON/SQLite sources)
// must yield an empty set rather than an error, so every rule's SQL stays
// identical across backends instead of branching.
func TestTestNodesView_EmptyWithoutAST(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "noast.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TEMP TABLE v_test_nodes AS " + testNodesViewSQL(false))
	require.NoError(t, err, "the no-AST form must be valid SQL on a db with no _ast at all")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_test_nodes`).Scan(&n))
	assert.Zero(t, n, "no _ast means no detectable test constructs, not an error")
}
