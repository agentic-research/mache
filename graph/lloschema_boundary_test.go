package graph

import "github.com/agentic-research/mache/internal/lloschema"

// The extraction's purpose, pinned (mache-be17ce).
//
// The canonical views used to live in internal/smells, which imports this
// package — so `graph` could not install them without a cycle, and that is the
// real reason the Go readers here still spell out ley-line-open's physical
// column names while the smell rules no longer do. lloschema declares its own
// single-method Querier rather than importing graph.RefsQuerier, which is what
// makes the dependency legal in this direction.
//
// Acyclicity itself needs no test — the compiler refuses an import cycle, and
// a test asserting it only improves the error message. What does need pinning
// is the STRUCTURAL match: Querier and graph.RefsQuerier are declared
// independently, in packages that cannot see each other's, so nothing but this
// line notices if one of them gains a method.
var _ lloschema.Querier = (*SQLiteGraph)(nil)
