package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// ensureCanonicalViews — ADR-0013 Step 3 expansion (mache-346d0f
// closeout). Tests pin the producer-detection logic: when ley-line-
// open's post-Step-1 _lsp_* tables are present (with referrer_node_id /
// ref_token / def_token columns), the views UNION ALL with binding-
// fidelity rows. When missing or pre-Step-1 schema, the views fall
// back to mention-only — same shape as PR #341.

// sqlDBQuerier wraps a *sql.DB as a refsQuerier. The smellTestGraph
// in serve_find_smells_test.go is heavier than these tests need
// (full graph + AST plumbing); a thin wrapper is enough.
type sqlDBQuerier struct{ db *sql.DB }

func (q *sqlDBQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

func TestEnsureCanonicalViews_BindingFidelityWhenLSPColumnsPresent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "binding.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Mention-fidelity tables (mache's own schema) + post-Step-1
	// _lsp_* shape (referrer_node_id, ref_token, def_token).
	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL,
			def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL,
			referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		-- Mention-fidelity: tree-sitter saw 'Validate' textually.
		INSERT INTO node_defs VALUES ('Validate', 'auth/functions/Validate');
		INSERT INTO node_refs VALUES ('Validate', 'billing/functions/Charge');

		-- Binding-fidelity: gopls resolved obj.Validate() at billing's
		-- byte range to auth/functions/Validate. ref_token = 'Validate'
		-- because that's the textual lemma at the source byte range.
		INSERT INTO _lsp_defs VALUES
			('auth/functions/Validate', 'Validate', 'file:///auth/auth.go', 10, 5, 10, 13);
		INSERT INTO _lsp_refs VALUES
			('auth/functions/Validate', 'billing/functions/Charge', 'Validate',
			 'file:///billing/billing.go', 42, 11, 42, 19);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// v_defs: one mention row + one binding row.
	type defRow struct {
		token, nodeID, fidelity string
	}
	rows, err := db.Query(`SELECT token, node_id, fidelity FROM v_defs ORDER BY fidelity, token`)
	require.NoError(t, err)
	var defs []defRow
	for rows.Next() {
		var r defRow
		require.NoError(t, rows.Scan(&r.token, &r.nodeID, &r.fidelity))
		defs = append(defs, r)
	}
	require.NoError(t, rows.Close())
	require.Len(t, defs, 2)
	assert.Equal(t, "binding", defs[0].fidelity)
	assert.Equal(t, "Validate", defs[0].token)
	assert.Equal(t, "auth/functions/Validate", defs[0].nodeID)
	assert.Equal(t, "mention", defs[1].fidelity)

	// v_refs: one mention row + one binding row. Binding row carries
	// the resolved target_node_id; mention row has NULL there.
	type refRow struct {
		referrer, token, fidelity string
		target                    sql.NullString
	}
	rrows, err := db.Query(`SELECT referrer_node_id, token, target_node_id, fidelity FROM v_refs ORDER BY fidelity, token`)
	require.NoError(t, err)
	var refs []refRow
	for rrows.Next() {
		var r refRow
		require.NoError(t, rrows.Scan(&r.referrer, &r.token, &r.target, &r.fidelity))
		refs = append(refs, r)
	}
	require.NoError(t, rrows.Close())
	require.Len(t, refs, 2)
	assert.Equal(t, "binding", refs[0].fidelity)
	assert.Equal(t, "billing/functions/Charge", refs[0].referrer)
	assert.Equal(t, "Validate", refs[0].token)
	assert.True(t, refs[0].target.Valid, "binding row must populate target_node_id")
	assert.Equal(t, "auth/functions/Validate", refs[0].target.String)
	assert.Equal(t, "mention", refs[1].fidelity)
	assert.False(t, refs[1].target.Valid, "mention row's target_node_id is NULL")
}

func TestEnsureCanonicalViews_HandlesMissingLSPTables(t *testing.T) {
	// .db without _lsp_* tables — what mache build produces today.
	// Views must still resolve and surface only mention rows.
	dbPath := filepath.Join(t.TempDir(), "no_lsp.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO node_defs VALUES ('Foo', 'pkg/Foo');
		INSERT INTO node_refs VALUES ('Foo', 'pkg/Bar');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	var defCount, refCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'mention'`).Scan(&defCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'mention'`).Scan(&refCount))
	assert.Equal(t, 1, defCount)
	assert.Equal(t, 1, refCount)

	// Querying for binding rows must succeed (the column exists in the
	// view shape) and return zero — no _lsp_* tables, no binding data.
	var bindingDefs, bindingRefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'binding'`).Scan(&bindingDefs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'binding'`).Scan(&bindingRefs))
	assert.Zero(t, bindingDefs, "no _lsp_defs → no binding rows")
	assert.Zero(t, bindingRefs, "no _lsp_refs → no binding rows")
}

func TestEnsureCanonicalViews_HandlesLegacyLSPTables(t *testing.T) {
	// Pre-Step-1 _lsp_* schema (no def_token / referrer_node_id /
	// ref_token columns). The probe must detect the absence and fall
	// back to mention-only views — querying the old _lsp_* via the
	// new view body would error with "no such column."
	dbPath := filepath.Join(t.TempDir(), "legacy_lsp.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		-- Pre-Step-1 shape: no def_token, no referrer_node_id, no ref_token.
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL, def_uri TEXT NOT NULL,
			def_start_line INTEGER, def_start_col INTEGER,
			def_end_line INTEGER, def_end_col INTEGER
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL, ref_uri TEXT NOT NULL,
			ref_start_line INTEGER, ref_start_col INTEGER,
			ref_end_line INTEGER, ref_end_col INTEGER
		);

		INSERT INTO node_defs VALUES ('Foo', 'pkg/Foo');
		INSERT INTO _lsp_defs VALUES ('pkg/Foo', 'file:///x.go', 1, 0, 1, 3);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// v_defs surfaces only the mention row. The legacy _lsp_defs row
	// is silently skipped — the consumer can't recover def_token from
	// it, so projecting it as binding-fidelity would misrepresent.
	var bindingDefs, mentionDefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'mention'`).Scan(&mentionDefs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'binding'`).Scan(&bindingDefs))
	assert.Equal(t, 1, mentionDefs)
	assert.Zero(t, bindingDefs, "legacy _lsp_defs (no def_token) must not produce binding rows")
}

func TestEnsureCanonicalViews_SkipsEmptyTokenRows(t *testing.T) {
	// LSP_REFS_DDL declares ref_token NOT NULL DEFAULT ''. Rows where
	// the producer couldn't extract the byte range (cross-repo refs,
	// missing source) carry empty tokens. The view body filters those
	// out — they'd contribute false matches in token-keyed rule
	// queries (especially dead_code's mention fallback arm).
	dbPath := filepath.Join(t.TempDir(), "empty_token.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL,
			def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL,
			referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		-- One def row with a real token (kept), one with empty (dropped).
		INSERT INTO _lsp_defs VALUES
			('pkg/Real',  'Real',  'file:///x.go', 1, 0, 1, 3),
			('pkg/Empty', '',      'file:///x.go', 5, 0, 5, 3);

		-- One ref with token + referrer (kept), one with empty token
		-- (dropped), one with NULL referrer (dropped).
		INSERT INTO _lsp_refs VALUES
			('pkg/Real',  'pkg/Caller',  'Real',  'file:///c.go', 9, 0, 9, 3),
			('pkg/Empty', 'pkg/Caller',  '',      'file:///c.go', 10, 0, 10, 3),
			('pkg/Real',  NULL,           'Real',  'file:///c.go', 11, 0, 11, 3);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	var bindingDefs, bindingRefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'binding'`).Scan(&bindingDefs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'binding'`).Scan(&bindingRefs))
	assert.Equal(t, 1, bindingDefs, "only the row with non-empty def_token survives")
	assert.Equal(t, 1, bindingRefs, "only the row with non-empty ref_token AND non-NULL referrer survives")
}

func TestTableHasColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE present (a TEXT, b INTEGER)`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}

	// Existing table, existing column.
	got, err := tableHasColumn(qg, "present", "a")
	require.NoError(t, err)
	assert.True(t, got)

	// Existing table, missing column — collapses with "table missing"
	// into false; consumer doesn't need to distinguish.
	got, err = tableHasColumn(qg, "present", "missing")
	require.NoError(t, err)
	assert.False(t, got)

	// Missing table — also false (PRAGMA table_info returns 0 rows).
	got, err = tableHasColumn(qg, "absent", "a")
	require.NoError(t, err)
	assert.False(t, got)

	// Injection-defense: invalid identifier rejected.
	_, err = tableHasColumn(qg, "no spaces; DROP TABLE present", "a")
	require.Error(t, err)
}
