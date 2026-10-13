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

// lspDDL returns the LSP-enrichment tables this fixture declares rows for, in
// the shape projection-v6 (keyed by nid) or v4 (keyed by node_id) writes them.
// They are not producer tables, so every dialect appends them.
func (b *Builder) lspDDL(v6 bool) []string {
	var out []string
	add := func(declared int, v4DDL, v6DDL []string) {
		switch {
		case declared == 0:
		case v6:
			out = append(out, v6DDL...)
		default:
			out = append(out, v4DDL...)
		}
	}
	add(len(b.lspDefs), []string{lspDefsTable}, lspDefsTableV6)
	add(len(b.lspSyms), lspSymbolsDDL("node_id TEXT"), lspSymbolsDDL("nid INTEGER"))
	add(len(b.lspHovers), lspHoverDDL("node_id TEXT"), lspHoverDDL("nid INTEGER"))
	return out
}

// lspSymbolsDDL and lspHoverDDL are `_lsp` and `_lsp_hover` as ley-line-open
// writes them, transcribed from LSP_DDL and LSP_HOVER_DDL in
// rs/ll-open/lsp/src/project.rs, at v0.19.1 for projection-v4 (key node_id
// TEXT) and v0.20.3 for v6 (key nid INTEGER); nothing else differs. Like
// _lsp_defs below, they are written by `leyline lsp`, which needs language
// servers a conformance run cannot assume.
func lspSymbolsDDL(key string) []string {
	return []string{
		`CREATE TABLE _lsp (
	` + key + ` PRIMARY KEY,
	symbol_kind TEXT,
	detail TEXT,
	start_line INTEGER NOT NULL, start_col INTEGER NOT NULL,
	end_line INTEGER NOT NULL, end_col INTEGER NOT NULL,
	diagnostics TEXT
)`,
		`CREATE INDEX idx_lsp_kind ON _lsp(symbol_kind)`,
	}
}

func lspHoverDDL(key string) []string {
	return []string{`CREATE TABLE _lsp_hover (
	` + key + ` PRIMARY KEY,
	hover_text TEXT NOT NULL
)`}
}

// lspDefsTableV6 is the same table as ley-line-open v0.20.x writes it for
// projection-v6, keyed by nid. Transcribed from LSP_DEFS_DDL in
// rs/ll-open/lsp/src/project.rs at v0.20.3: the table is written by `leyline
// lsp`, which needs language servers a conformance run cannot assume, so this
// one is not re-derived from a binary.
var lspDefsTableV6 = []string{
	`CREATE TABLE _lsp_defs (
	nid INTEGER NOT NULL,
	def_token TEXT NOT NULL DEFAULT '',
	def_uri TEXT NOT NULL,
	def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
	def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
)`,
	`CREATE INDEX idx_lsp_defs_node ON _lsp_defs(nid)`,
	`CREATE INDEX idx_lsp_defs_token ON _lsp_defs(def_token)`,
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
