package fixturedb

import (
	"database/sql"
	"maps"
	"path/filepath"
	"slices"
	"testing"
)

// FixtureDB is a built fixture: a real on-disk SQLite database whose shape is the
// producer's, with the canonical views already installed.
//
// It satisfies [RefsQuerier] and cmd's dbPathProvider, so it drops directly into
// the production call path.
type FixtureDB struct {
	t    *testing.T
	db   *sql.DB
	path string
}

// QueryRefs implements [RefsQuerier].
func (f *FixtureDB) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return f.db.Query(query, args...)
}

// DBPath implements the dbPathProvider opt-in, which is what enables capnp
// binding readthrough from the sibling .bindings.capnp log.
func (f *FixtureDB) DBPath() string { return f.path }

// DB exposes the connection so a test can assert on rows directly.
//
// It is NOT an escape hatch for DDL: the fixture's shape is already fixed by the
// producer, and internal/lint's LLO boundary rule fails any test that writes an
// LLO-owned table.
func (f *FixtureDB) DB() *sql.DB { return f.db }

// Build materialises the fixture and returns its path alongside it.
//
// The connection is capped at ONE so the TEMP views survive across queries —
// TEMP objects are per-connection, and a fixture whose pool hands out a second
// connection loses v_defs / v_refs / v_test_nodes non-deterministically. Getting
// that wrong was a per-fixture coin flip before this package; now it is settled
// in one place.
//
// The canonical views are installed here, not by the caller, so a test cannot
// forget them.
func (b *Builder) Build() (string, *FixtureDB) {
	t := b.t
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("fixturedb: open %s: %v", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	for _, stmt := range b.schemaStatements() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixturedb(%s): create schema: %v\n%s", b.producer, err, stmt)
		}
	}
	b.insertRows(db)

	f := &FixtureDB{t: t, db: db, path: dbPath}
	if viewInstaller != nil {
		if err := viewInstaller(f); err != nil {
			t.Fatalf("fixturedb(%s): install canonical views: %v", b.producer, err)
		}
	}
	return dbPath, f
}

// schemaStatements returns the producer's DDL, plus the ley-line-owned
// `_ast` / `_source` / `node_content` / `_imports` tables when this fixture
// carries rows for them.
//
// Presence is deliberately CONDITIONAL on [Standalone] and unconditional on
// [Leyline], because that is what the producers do — and because
// ensureCanonicalViews PROBES for `_ast`: creating it unconditionally would flip
// every Standalone fixture onto the v_test_nodes arm that real mache .db files
// never reach.
func (b *Builder) schemaStatements() []string {
	var stmts []string
	add := func(names []string, from map[string]string) {
		for _, n := range names {
			stmts = append(stmts, from[n])
		}
	}

	switch b.producer {
	case Leyline:
		add(leylineTableOrder, leylineTables)
		add(slices.Sorted(maps.Keys(leylineIndexes)), leylineIndexes)
	default: // Standalone
		add(standaloneTableOrder, standaloneTables)
		add(slices.Sorted(maps.Keys(standaloneIndexes)), standaloneIndexes)
		add(slices.Sorted(maps.Keys(standaloneViews)), standaloneViews)
		// The cache-hydration path (cmd/cache.go) materialises ley-line's
		// parse output onto a mache-projection .db. Model it only when the
		// fixture actually declares such rows.
		if len(b.ast) > 0 {
			stmts = append(stmts, leylineTables["node_content"], leylineTables["_ast"])
		}
		if len(b.sources) > 0 {
			stmts = append(stmts, leylineTables["_source"])
		}
	}
	if len(b.lspDefs) > 0 {
		stmts = append(stmts, lspDefsTable)
	}
	return stmts
}

// lspDefsTable is the LSP-enrichment def table ley-line's ll-open/lsp crate
// writes. Not a producer table: it is optional on both producers.
const lspDefsTable = `CREATE TABLE _lsp_defs (
	node_id TEXT NOT NULL,
	def_token TEXT NOT NULL DEFAULT '',
	def_uri TEXT NOT NULL,
	def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
	def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
)`
