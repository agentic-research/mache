package fixturedb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// FixtureDB is a built fixture: a real on-disk SQLite database whose shape is the
// producer's, with the canonical views already installed.
//
// It satisfies [RefsQuerier] and cmd's dbPathProvider, so it drops directly into
// the production call path.
type FixtureDB struct {
	t    testing.TB
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

	for _, stmt := range b.producer.dialect.schema(b) {
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

// lspDDL returns the LSP-enrichment table when this fixture declares rows for
// it. It is not a producer table, so every dialect appends it.
func (b *Builder) lspDDL() []string {
	if len(b.lspDefs) > 0 {
		return []string{lspDefsTable}
	}
	return nil
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
