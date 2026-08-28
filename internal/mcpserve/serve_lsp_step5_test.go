package mcpserve

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// sqlDBQuerier adapts a *sql.DB (+ optional .db path for capnp readthrough)
// to the refs-querier shape handlers expect. Deliberately duplicated from
// internal/smells' test double (12 lines, stage-boundary duplication the
// decomposition plan accepts); the serve-side tests carry it to mcpserve in
// stage 8.
type sqlDBQuerier struct {
	db   *sql.DB
	path string
}

func (q *sqlDBQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

func (q *sqlDBQuerier) DBPath() string { return q.path }

// queryLSPDefs / queryLSPRefs — ADR-0013 Step 5 (mache-346d2b).
// Tests pin the dispatch: when ley-line-open's post-Step-1 columns
// (def_token / ref_token) are present, the queries use direct token
// match. When absent, they fall through to the legacy
// suffix-then-broader-LIKE pattern. Existing fixtures in serve_test.go
// already exercise the legacy path; this file covers the new path
// and the boundary between them.

// stepOneLSPDB builds a fixture with the post-Step-1 _lsp_* schema —
// def_token populated on _lsp_defs and (referrer_node_id, ref_token)
// populated on _lsp_refs. Returns the *sql.DB wrapped as a graph.RefsQuerier.
func stepOneLSPDB(t *testing.T) *sqlDBQuerier {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "step1.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
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

		-- Two defs of 'Read': one on MyReader, one on OtherReader.
		-- Direct token match must surface BOTH (without disambiguating
		-- by node_id) — same semantic as today's broader-LIKE fallback.
		INSERT INTO _lsp_defs VALUES
			('pkg/methods/MyReader.Read',    'Read', 'file:///pkg/my.go',    10, 5, 10, 9),
			('pkg/methods/OtherReader.Read', 'Read', 'file:///pkg/other.go', 20, 5, 20, 9),
			('pkg/functions/Validate',       'Validate', 'file:///pkg/v.go',  5, 5,  5, 13);

		-- Refs to Read from two different referrers, plus a ref to
		-- Validate from one referrer.
		INSERT INTO _lsp_refs VALUES
			('pkg/methods/MyReader.Read',    'pkg/functions/Caller1', 'Read',     'file:///pkg/c1.go', 30, 11, 30, 15),
			('pkg/methods/OtherReader.Read', 'pkg/functions/Caller2', 'Read',     'file:///pkg/c2.go', 40, 11, 40, 15),
			('pkg/functions/Validate',       'pkg/functions/Caller3', 'Validate', 'file:///pkg/c3.go', 50, 11, 50, 19);
	`)
	require.NoError(t, err)
	return &sqlDBQuerier{db: db}
}

func TestQueryLSPDefs_DirectTokenMatch_Step1(t *testing.T) {
	qg := stepOneLSPDB(t)

	// Direct match on def_token. Must return BOTH Read defs (the
	// node_id LIKE '%/Read' suffix would also match both, but only
	// because Read happens to be the trailing component — for
	// methods rendered as 'Receiver.Method' the suffix would NOT
	// match and the broader fallback would be needed. The new
	// direct match handles methods correctly without the fallback.)
	defs, err := queryLSPDefs(qg, "Read")
	require.NoError(t, err)
	require.Len(t, defs, 2)

	uris := []string{defs[0].URI, defs[1].URI}
	assert.Contains(t, uris, "file:///pkg/my.go")
	assert.Contains(t, uris, "file:///pkg/other.go")
}

func TestQueryLSPDefs_DirectTokenMatch_NoBroaderLIKE(t *testing.T) {
	// Defining a func 'Bar' and a func 'BarHelper' — direct token
	// match on 'Bar' returns only the Bar def, NOT BarHelper. The
	// legacy broader-LIKE ('%Bar%') would match both; the new direct
	// match is more precise.
	dbPath := filepath.Join(t.TempDir(), "narrow.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL,
			def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		INSERT INTO _lsp_defs VALUES
			('pkg/functions/Bar',       'Bar',       'file:///x.go', 1, 0, 1, 3),
			('pkg/functions/BarHelper', 'BarHelper', 'file:///x.go', 5, 0, 5, 9);
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}

	defs, err := queryLSPDefs(qg, "Bar")
	require.NoError(t, err)
	require.Len(t, defs, 1, "direct token match must NOT spuriously match 'BarHelper'")
	assert.Equal(t, "pkg/functions/Bar", defs[0].NodeID)
}

func TestQueryLSPRefs_DirectTokenMatch_Step1(t *testing.T) {
	qg := stepOneLSPDB(t)

	refs, err := queryLSPRefs(qg, "Read")
	require.NoError(t, err)
	require.Len(t, refs, 2, "two refs to 'Read' from distinct callers")

	uris := []string{refs[0].URI, refs[1].URI}
	assert.Contains(t, uris, "file:///pkg/c1.go")
	assert.Contains(t, uris, "file:///pkg/c2.go")
}

func TestQueryLSPDefs_TableMissing_StillReturnsNilNil(t *testing.T) {
	// Contract preservation: a .db without _lsp_defs returns
	// (nil, nil). The Step-5 migration must keep this invariant
	// because callers (find_definition handler) treat nil as "no
	// LSP enrichment available, fall back to ordinary def lookup."
	dbPath := filepath.Join(t.TempDir(), "no_lsp.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	qg := &sqlDBQuerier{db: db}

	defs, err := queryLSPDefs(qg, "Anything")
	require.NoError(t, err)
	assert.Nil(t, defs, "no _lsp_defs table → nil result, no error")

	refs, err := queryLSPRefs(qg, "Anything")
	require.NoError(t, err)
	assert.Nil(t, refs, "no _lsp_refs table → nil result, no error")
}

func TestRefsTableExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exists.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE present (a TEXT)`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}

	got, err := refsTableExists(qg, "present")
	require.NoError(t, err)
	assert.True(t, got)

	got, err = refsTableExists(qg, "absent")
	require.NoError(t, err)
	assert.False(t, got)

	// Injection-defense: rejects names that aren't simple identifiers.
	_, err = refsTableExists(qg, "evil; DROP TABLE present")
	require.Error(t, err)
}
