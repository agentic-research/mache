package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/ley-line-open/clients/go/leyline-schema/binding"
	"github.com/agentic-research/mache/internal/lsp"
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

// TestEnsureCanonicalViews_DefBindingFidelityFromLSPSQL pins the
// def-side binding arm, which still reads from the SQL `_lsp_defs`
// table (BindingRecord covers refs only — the def-side migration is
// tracked separately, post-mache-6bd4d8). The ref-side binding arm
// is exercised by TestLoadCapnpBindings_PopulatesViewFromSiblingLog,
// which uses the sibling .bindings.capnp event log instead.
func TestEnsureCanonicalViews_DefBindingFidelityFromLSPSQL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "binding.db")
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

		INSERT INTO node_defs VALUES ('Validate', 'auth/functions/Validate');
		INSERT INTO _lsp_defs VALUES
			('auth/functions/Validate', 'Validate', 'file:///auth/auth.go', 10, 5, 10, 13);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

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
	require.Len(t, defs, 2, "one mention row from node_defs + one binding row from _lsp_defs")
	assert.Equal(t, "binding", defs[0].fidelity)
	assert.Equal(t, "Validate", defs[0].token)
	assert.Equal(t, "auth/functions/Validate", defs[0].nodeID)
	assert.Equal(t, "mention", defs[1].fidelity)
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

// TestEnsureCanonicalViews_ReferrerFromContainerNodeID pins the leyline
// caller-aggregation fix (mache-ba3dc6): when node_refs carries the additive
// container_node_id column (ley-line-open v0.7.4+, the nearest enclosing def),
// v_refs.referrer_node_id resolves to it so caller-aggregating rules
// (fan_out_skew) group by the real caller instead of the unique call-site
// node_id. Empty/NULL containers fall back to node_id.
func TestEnsureCanonicalViews_ReferrerFromContainerNodeID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "container.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT);
		CREATE TABLE node_refs (token TEXT, node_id TEXT, container_node_id TEXT);
		-- two calls from inside the SAME enclosing function (its def node),
		-- plus one ref whose container is empty (top-level / file scope).
		INSERT INTO node_refs VALUES
			('helperA', 'f.go/function_declaration/call_expression_0', 'f.go/function_declaration'),
			('helperB', 'f.go/function_declaration/call_expression_1', 'f.go/function_declaration'),
			('toplevel', 'f.go/call_expression_top', '');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// Both calls resolve to the ONE enclosing def → caller aggregation sees a
	// fan-out of 2, not two singletons (the bug: call-site node_id is unique).
	var distinctReferrers, callsFromEnclosing int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(DISTINCT referrer_node_id) FROM v_refs WHERE fidelity='mention' AND token IN ('helperA','helperB')`).Scan(&distinctReferrers))
	assert.Equal(t, 1, distinctReferrers, "both calls resolve to the one enclosing def via container_node_id")
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM v_refs WHERE referrer_node_id='f.go/function_declaration'`).Scan(&callsFromEnclosing))
	assert.Equal(t, 2, callsFromEnclosing)

	// Empty container falls back to the call-site node_id.
	var topReferrer string
	require.NoError(t, db.QueryRow(
		`SELECT referrer_node_id FROM v_refs WHERE token='toplevel'`).Scan(&topReferrer))
	assert.Equal(t, "f.go/call_expression_top", topReferrer, "empty container_node_id falls back to node_id")
}

// TestEnsureCanonicalViews_QualifierFromNodeRefs pins the node_refs.qualifier
// passthrough (ley-line-open v0.8.0, mache-dcb808): the mention arm surfaces
// the ref's qualifier (receiver/selector text) instead of the pre-v0.8.0
// hardcoded ”. Legacy dbs without the column degrade to ”.
func TestEnsureCanonicalViews_QualifierFromNodeRefs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "qual.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT);
		CREATE TABLE node_refs (token TEXT, node_id TEXT, qualifier TEXT);
		INSERT INTO node_refs VALUES
			('Println', 'f.go/call_0', 'fmt'),
			('Method',  'f.go/call_1', 'obj'),
			('bareCall','f.go/call_2', NULL);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	var q string
	require.NoError(t, db.QueryRow(`SELECT qualifier FROM v_refs WHERE token='Println'`).Scan(&q))
	assert.Equal(t, "fmt", q, "mention arm must surface node_refs.qualifier")
	require.NoError(t, db.QueryRow(`SELECT qualifier FROM v_refs WHERE token='Method'`).Scan(&q))
	assert.Equal(t, "obj", q)
	require.NoError(t, db.QueryRow(`SELECT qualifier FROM v_refs WHERE token='bareCall'`).Scan(&q))
	assert.Equal(t, "", q, "NULL qualifier degrades to empty string")
}

// TestEnsureCanonicalViews_QualifierAbsentDegradesToEmpty pins the legacy
// path: a node_refs WITHOUT a qualifier column must still build v_refs with
// an empty qualifier (no error).
func TestEnsureCanonicalViews_QualifierAbsentDegradesToEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "noqual.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT);
		CREATE TABLE node_refs (token TEXT, node_id TEXT);
		INSERT INTO node_refs VALUES ('x', 'f.go/call_0');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg), "missing qualifier column must not error")

	var q string
	require.NoError(t, db.QueryRow(`SELECT qualifier FROM v_refs WHERE token='x'`).Scan(&q))
	assert.Equal(t, "", q)
}

// TestEnsureCanonicalViews_ReferrerFallsBackWithoutContainer pins the
// legacy/tree-sitter shape: node_refs WITHOUT a container_node_id column →
// referrer_node_id is node_id (the mache-schema caller construct), unchanged.
func TestEnsureCanonicalViews_ReferrerFallsBackWithoutContainer(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_refs.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT);
		CREATE TABLE node_refs (token TEXT, node_id TEXT);
		INSERT INTO node_refs VALUES ('Foo', 'functions/Caller/source');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	var referrer string
	require.NoError(t, db.QueryRow(
		`SELECT referrer_node_id FROM v_refs WHERE token='Foo'`).Scan(&referrer))
	assert.Equal(t, "functions/Caller/source", referrer, "no container_node_id column → referrer is node_id")
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

func TestEnsureCanonicalViews_SkipsEmptyDefTokenRows(t *testing.T) {
	// _lsp_defs.def_token is NOT NULL DEFAULT ''. Rows where the
	// producer couldn't extract a token (cross-repo defs, missing
	// source) carry empty tokens. v_defs's binding arm filters them
	// out — they'd contribute false matches in token-keyed rule
	// queries (dead_code's mention fallback arm in particular).
	//
	// Post-mache-6bd4d8 this assertion is def-side only. The
	// equivalent ref-side filter is now enforced at the producer
	// boundary (LLO's BindingRecord generation skips empty refToken)
	// — see TestReadBindingLog_RealLLOOutput which pins refToken
	// non-empty as a producer invariant.
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

		-- One def row with a real token (kept), one with empty (dropped).
		INSERT INTO _lsp_defs VALUES
			('pkg/Real',  'Real',  'file:///x.go', 1, 0, 1, 3),
			('pkg/Empty', '',      'file:///x.go', 5, 0, 5, 3);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	var bindingDefs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs WHERE fidelity = 'binding'`).Scan(&bindingDefs))
	assert.Equal(t, 1, bindingDefs, "only the row with non-empty def_token survives")
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
		rec, err := binding.NewRootBindingRecord(seg)
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
