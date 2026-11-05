package fixturedb

import (
	"database/sql"
	"testing"

	"github.com/agentic-research/mache/internal/sqlintro"
)

// emitter carries the per-build state the row writers share.
type emitter struct {
	// derivedParent is true when the producer's nodes.parent_id is a GENERATED
	// column, which ley-line-open made it in projection-v4 (v0.19.0). Naming a
	// generated column in an INSERT is rejected at PREPARE time, so the column
	// LIST has to differ — the value does not, since the derivation reproduces
	// exactly what this emitter would have written.
	derivedParent bool
	b             *Builder
	db            *sql.Tx
	// hasNodeContent reports whether this fixture has a node_content table to
	// point node_hash values at. Ley-line always does; a Standalone fixture only
	// does when it modelled the cache-hydration path.
	hasNodeContent bool

	// v6 and v6Sites are the projection-v6 dialect's tree and its call-site
	// nodes (one per ref, in b.refs order), built once on first use. Unused by
	// the v4 dialects.
	v6      *v6Tree
	v6Sites []*v6Node
}

// insertRows writes every spec inside ONE transaction: a fixture is
// all-or-nothing, and a benchmark-sized one (thousands of `_ast` rows) costs
// a single fsync rather than one per row.
func (b *Builder) insertRows(db *sql.DB) {
	b.t.Helper()
	tx, err := db.Begin()
	if err != nil {
		b.t.Fatalf("fixturedb(%s): begin: %v", b.producer, err)
	}
	d := b.producer.dialect
	e := &emitter{
		b: b, db: tx,
		hasNodeContent: d.hasNodeContent(b),
		derivedParent:  sqlintro.ColumnIsGenerated(tx, "nodes", "parent_id"),
	}
	d.emitNodes(e)
	d.emitSymbols(e)
	d.emitAST(e)
	d.emitSources(e)
	d.emitImports(e)
	e.emitLSP()
	if err := tx.Commit(); err != nil {
		b.t.Fatalf("fixturedb(%s): commit: %v", b.producer, err)
	}
}

func (e *emitter) exec(q string, args ...any) {
	e.b.t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		e.b.t.Fatalf("fixturedb(%s): %v\n%s", e.b.producer, err, q)
	}
}

func mustExec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("fixturedb: %v\n%s", err, q)
	}
}

// BlobDamage is a way a stored blob can be wrong, for tests of mache's
// read-time verification (mache-c1eb7a); DamageBlob applies it.
type BlobDamage int

const (
	// BlobTampered replaces the bytes under the hash, leaving the hash: what a
	// corrupted or hostile store would serve.
	BlobTampered BlobDamage = iota
	// BlobChunked makes the bytes NULL, as a chunk-activated store holds them.
	BlobChunked
	// BlobDropped deletes the row: a hash the store does not hold.
	BlobDropped
)
