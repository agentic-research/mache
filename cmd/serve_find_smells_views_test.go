package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/mache/internal/lsp"
	"github.com/agentic-research/mache/internal/lsp/bindings"
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
type sqlDBQuerier struct {
	db   *sql.DB
	path string // optional: when set, exposed via DBPath() for capnp readthrough
}

func (q *sqlDBQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

// DBPath implements dbPathProvider when path is set, opting this
// querier into the capnp-readthrough path. Tests that don't set path
// keep the legacy mention + SQL-binding view shape.
func (q *sqlDBQuerier) DBPath() string { return q.path }

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

// TestLoadCapnpBindings_PopulatesViewFromSiblingLog asserts the
// step 3 capnp readthrough end-to-end: ensureCanonicalViews creates
// the empty _capnp_binding_refs TEMP table, LoadCapnpBindings reads
// the sibling .bindings.capnp event log and inserts records, and
// the v_refs UNION arm surfaces them as binding-fidelity rows
// alongside the mention-only node_refs rows.
func TestLoadCapnpBindings_PopulatesViewFromSiblingLog(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "self.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// Per-connection TEMP table — pin to one connection so the table
	// LoadCapnpBindings populates is the same one the v_refs SELECT
	// reads from.
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO node_refs VALUES ('Validate', 'billing/functions/Charge');
	`)
	require.NoError(t, err)

	// Write a sibling .bindings.capnp with two records: one for the
	// same Validate call billing makes (so we get a binding-fidelity
	// confirmation alongside the mention) and one for a totally new
	// reference (so we know the capnp arm is genuinely contributing
	// rows the SQL doesn't have).
	logPath := lsp.SiblingBindingLogPath(dbPath)
	f, err := os.Create(logPath)
	require.NoError(t, err)
	enc := capnp.NewEncoder(f)
	for _, r := range []struct {
		target, token, construct, refSite, uri string
		startLine                              uint32
	}{
		{"auth/functions/Validate", "Validate", "billing/functions/Charge", "billing/.../field_identifier", "file:///billing.go", 42},
		{"stdlib/io/Reader.Read", "Read", "pkg/functions/Loop", "pkg/.../field_identifier", "file:///pkg.go", 17},
	} {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		require.NoError(t, err)
		rec, err := bindings.NewRootBindingRecord(seg)
		require.NoError(t, err)
		require.NoError(t, rec.SetTargetNodeId(r.target))
		require.NoError(t, rec.SetRefToken(r.token))
		require.NoError(t, rec.SetConstructNodeId(r.construct))
		require.NoError(t, rec.SetRefSiteNodeId(r.refSite))
		require.NoError(t, rec.SetRefUri(r.uri))
		rng, err := rec.NewRefRange()
		require.NoError(t, err)
		start, err := rng.NewStart()
		require.NoError(t, err)
		start.SetLine(r.startLine)
		_, err = rng.NewEnd()
		require.NoError(t, err)
		require.NoError(t, enc.Encode(msg))
	}
	require.NoError(t, f.Close())

	qg := &sqlDBQuerier{db: db, path: dbPath}
	require.NoError(t, ensureCanonicalViews(qg))
	require.NoError(t, LoadCapnpBindings(qg, qg.DBPath()))

	// v_refs should now surface 1 mention row + 2 binding rows.
	var mentionRefs, bindingRefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'mention'`).Scan(&mentionRefs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'binding'`).Scan(&bindingRefs))
	assert.Equal(t, 1, mentionRefs, "mention arm preserved (node_refs row)")
	assert.Equal(t, 2, bindingRefs, "capnp arm contributes both records")

	// Pin the projection: capnp's constructNodeId becomes
	// referrer_node_id in v_refs (matches node_refs.node_id shape).
	// This is the structural fix for Falsifiability B.
	rows, err := db.Query(`SELECT referrer_node_id, token, target_node_id
	                       FROM v_refs WHERE fidelity = 'binding' ORDER BY token`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type bindingRow struct{ ref, tok, tgt string }
	var got []bindingRow
	for rows.Next() {
		var b bindingRow
		require.NoError(t, rows.Scan(&b.ref, &b.tok, &b.tgt))
		got = append(got, b)
	}
	require.Equal(t, []bindingRow{
		{"pkg/functions/Loop", "Read", "stdlib/io/Reader.Read"},
		{"billing/functions/Charge", "Validate", "auth/functions/Validate"},
	}, got, "constructNodeId → referrer_node_id projection (the Falsifiability B fix)")
}

// TestLoadCapnpBindings_NoSiblingLogIsNoOp asserts the function is a
// silent no-op when the sibling .bindings.capnp file doesn't exist.
// LLO doesn't write the log when no LSP-resolved refs exist; mache
// must treat that as "no enrichment", not as an error.
func TestLoadCapnpBindings_NoSiblingLogIsNoOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "no-log.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO node_refs VALUES ('Foo', 'pkg/functions/Bar');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db, path: dbPath}
	require.NoError(t, ensureCanonicalViews(qg))
	// Should not error even though no sibling .bindings.capnp exists.
	require.NoError(t, LoadCapnpBindings(qg, qg.DBPath()))

	var bindingRefs, mentionRefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'binding'`).Scan(&bindingRefs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'mention'`).Scan(&mentionRefs))
	assert.Equal(t, 0, bindingRefs, "no capnp log → no binding rows")
	assert.Equal(t, 1, mentionRefs, "mention arm still works")
}

// TestLoadCapnpBindings_EmptyDBPathIsNoOp asserts a refsQuerier
// without a known path (e.g. in-memory test fixtures) skips the
// capnp readthrough silently. This is the "querier doesn't implement
// dbPathProvider" branch as exercised through the CLI/MCP entry
// points.
func TestLoadCapnpBindings_EmptyDBPathIsNoOp(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "empty.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))
	require.NoError(t, LoadCapnpBindings(qg, ""))

	var bindingRefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs WHERE fidelity = 'binding'`).Scan(&bindingRefs))
	assert.Equal(t, 0, bindingRefs, "no path → silent skip, no binding rows")
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
