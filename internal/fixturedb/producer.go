// Package fixturedb builds SQLite test fixtures whose TABLE SHAPE is the shape
// a real producer writes, and removes a test's ability to state a schema at all.
//
// # Why this exists
//
// Before this package, ~34 test files hand-wrote `CREATE TABLE node_refs (...)`,
// in ten mutually incompatible spellings, and zero of them reproduced ley-line's
// real shape. That was not a cosmetic problem. `ensureCanonicalViews` PROBES for
// columns and emits a structurally DIFFERENT `v_refs` / `v_defs` body per
// combination — most sharply:
//
//	refsReferrerExpr := "node_id"
//	if hasRefsContainer {
//	    refsReferrerExpr = "COALESCE(NULLIF(container_node_id, ''), node_id)"
//	}
//
// So a fixture's `CREATE TABLE` literal was a HIDDEN TEST PARAMETER selecting
// which SQL the rule under test actually executed. A rule tested against a
// two-column node_refs exercised the degraded arm while production exercised the
// full arm, and neither side noticed.
//
// Compounding it, `node_refs.node_id` has two incompatible meanings: the
// mache/tree-sitter projection puts the ENCLOSING CONSTRUCT there; ley-line puts
// the CALL-SITE LEAF there and the enclosing def in `container_node_id`. A
// fixture picked a meaning implicitly, by what it INSERTed, with nowhere in the
// test saying which.
//
// This is the same defect class as mache-e3d9bb (`Node.Children` had two
// accessors that silently disagreed; the fix was removing the second way to
// ask), one layer down. The fix shape is the same: remove the second way to
// state a schema.
//
// # The three properties that carry the weight
//
//  1. NO method on Builder accepts SQL, DDL, a column name, or a table name.
//     That absence IS the design. The DDL in schema.go is unexported and there
//     is no escape hatch.
//
//  2. [Builder.Ref] takes `from` and `at` as SEPARATE TYPED PARAMETERS, so the
//     two meanings of node_id become distinct arguments instead of an undeclared
//     convention. [Leyline] writes node_id=<at>, container_node_id=<from>;
//     [Standalone] writes node_id=<from> and structurally cannot express <at>.
//
//  3. [Builder.Build] returns a fixture already wired through the canonical view
//     installer, so a test cannot forget the install (a real past problem — see
//     the note in cmd/smell_refs_views.go about "the 26 tests that build
//     fixtures by hand").
//
// # Provenance
//
// The [Leyline] DDL is DERIVED, not hand-written: see schema_leyline.go and the
// conformance test that re-derives it from the pinned binary and fails on drift.
// The [Standalone] DDL is likewise conformance-tested against what
// ingest.NewSQLiteWriter actually creates.
package fixturedb

// Producer names the tool whose table shape a fixture reproduces.
//
// The set is CLOSED: the only values are [Leyline] and [Standalone]. The single
// field is unexported, so the zero value is invalid and a caller outside this
// package cannot construct a third. A fixture therefore cannot exist without
// naming the producer it models — which is precisely what the 34 hand-written
// fixtures never did.
type Producer struct{ name string }

var (
	// Leyline is the ley-line-open parse output: node_refs carries
	// source_id / container_node_id / qualifier / node_hash and has NO
	// primary key, so duplicate (token, node_id) rows SURVIVE. node_id is
	// the call-SITE leaf; the enclosing definition is container_node_id.
	// This is the shape production reads.
	Leyline = Producer{name: "leyline"}

	// Standalone is mache's own schema projection (internal/ingest.SQLiteWriter):
	// node_refs is (token, node_id) with PRIMARY KEY (token, node_id) WITHOUT
	// ROWID, so duplicate rows are DEDUPED. node_id is the ENCLOSING CONSTRUCT;
	// there is no place to put a call site or a qualifier.
	//
	// The dedupe is not incidental. Any COUNT/AVG-over-v_refs rule measures a
	// different quantity here than on [Leyline] — the fan_out_skew class
	// (mache-50e939).
	Standalone = Producer{name: "standalone"}
)

// String returns the producer's name, for test failure messages.
func (p Producer) String() string {
	if p.name == "" {
		return "invalid(zero-value Producer)"
	}
	return p.name
}

// valid reports whether p is one of the two package-declared producers rather
// than the zero value. Only [New] consults it.
func (p Producer) valid() bool { return p.name != "" }
