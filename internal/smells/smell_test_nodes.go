package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
)

// testNodesViewSQL builds `v_test_nodes` — the single definition of "this
// construct is test code", consumed by every rule that should not judge tests
// by production standards.
//
// WHY A VIEW AND NOT A PER-RULE FILTER. Rules already carry Go-shaped test
// exclusions written independently: dead_code skips `%_test.go` per file and
// Test*/Benchmark*/Example*/Fuzz* per construct, fan_out_skew repeats the same
// two, god_file has its own. Each was correct for Go and silently wrong for
// every other language, and fixing them one at a time reproduces the drift.
// One view means one answer, and adding a language means editing one place.
//
// WHY RUST NEEDED THIS AT ALL. Go states test-ness in the FILE NAME, so
// `source_file LIKE '%_test.go'` is a complete answer. Rust states it in an
// ATTRIBUTE — `#[cfg(test)] mod tests` and `#[test] fn` — and colocates the
// result in the production file, so no path or name predicate can see it.
// Measured on ley-line-open/rs: 46% of files carry #[cfg(test)] and 34.5% of
// all source bytes sit inside one (mache-c7d56d). Those constructs were being
// judged as production API by dead_code, which is ~1986 of its findings there
// (mache-8ecfd7 part 1).
//
// HOW IT IS DETECTED, structurally rather than textually.
//
// In tree-sitter-rust an `attribute_item` is a SIBLING of the item it
// decorates, not a child, so a descendant query cannot associate them:
//
//	row 486  attribute_item     <- #[test]
//	row 487  function_item      <- the function it applies to
//
// So the association is "the first node beginning at or after the attribute's
// end_byte", which resolves correctly for both shapes (verified on real
// output: attribute at row 478 -> mod_item at 479; attribute at 486 ->
// function_item at 487).
//
// The attribute's CONTENT is read from `node_content.token` joined on
// node_hash — not by slicing source text. `_source.content` is NULL in the
// parse output (in v0.10.3 as well as v0.11.3, so this is not a CDC or version
// artifact — content simply is not stored there), and matching tokens is
// structural, which is what this repo prefers over textual heuristics anyway.
//
// Both `#[test]` and `#[cfg(test)]` reduce to "an attribute_item containing an
// identifier whose token is `test`". That deliberately conflates them: one
// marks a test function, the other a test module, and for the purpose of "do
// not judge this as production code" they mean the same thing.
//
// TRANSITIVITY IS THE POINT. `#[cfg(test)] mod tests { ... }` makes everything
// inside it test code, not just the mod node. Containment is by byte range
// within one source, which is exact for a well-formed parse.
//
// DEGRADES TO EMPTY, NEVER ERRORS. A projection without `_ast` (mache's own
// schema projections, JSON/SQLite sources) yields no rows, and a rule that
// filters against an empty view behaves exactly as it does today. That matters
// because these views are installed for every find-smells run regardless of
// backend.
func testNodesViewSQL(hasAST bool) string {
	if !hasAST {
		// No _ast: nothing to detect. An empty view keeps every consumer's SQL
		// identical rather than making them branch on backend.
		return `SELECT '' AS source_id, '' AS node_id WHERE 0`
	}
	return `
		WITH test_attrs AS (
			-- attribute_item nodes that are #[test] or #[cfg(test)]:
			-- an attribute carrying an identifier whose token is 'test'.
			SELECT a.source_id, a.end_byte
			FROM _ast a
			WHERE a.node_kind = 'attribute_item'
			  AND EXISTS (
				SELECT 1 FROM _ast d
				JOIN node_content nc ON nc.node_hash = d.node_hash
				WHERE d.source_id = a.source_id
				  AND d.node_kind = 'identifier'
				  AND d.start_byte >= a.start_byte
				  AND d.end_byte   <= a.end_byte
				  AND nc.token = 'test'
			  )
		),
		-- The decorated item: the first function_item/mod_item beginning at or
		-- after the attribute's end. Attributes are SIBLINGS of what they
		-- decorate, so position is the association.
		--
		-- Filtering to those two kinds (rather than taking the next node of any
		-- kind) does two things: it skips intervening attribute_items, so
		-- stacked attributes (a cfg(test) followed by an allow(...) before
		-- the mod) still resolve to the mod; and it turns a correlated per-attribute
		-- scan of a 250k-row table into one filtered aggregate join. The naive
		-- MIN(start_byte) over all node kinds was both wrong on stacking and
		-- too slow to run inside the smell gate.
		roots AS (
			SELECT a.source_id, a.node_id, a.start_byte, a.end_byte
			FROM _ast a
			JOIN (
				SELECT t.source_id, t.end_byte,
				       MIN(n.start_byte) AS target_start
				FROM test_attrs t
				JOIN _ast n
				  ON n.source_id = t.source_id
				 AND n.node_kind IN ('function_item', 'mod_item')
				 AND n.start_byte >= t.end_byte
				GROUP BY t.source_id, t.end_byte
			) d
			  ON d.source_id = a.source_id
			 AND d.target_start = a.start_byte
			WHERE a.node_kind IN ('function_item', 'mod_item')
		)
		-- The decorated node itself, plus everything contained within it:
		-- #[cfg(test)] mod makes its whole body test code.
		SELECT DISTINCT a.source_id, a.node_id
		FROM _ast a
		JOIN roots r
		  ON r.source_id = a.source_id
		 AND a.start_byte >= r.start_byte
		 AND a.end_byte   <= r.end_byte`
}

// ensureTestNodesView installs v_test_nodes. Best-effort in the same sense as
// the canonical views: a failure to build it must not fail the run, because a
// missing test-exclusion degrades to today's behaviour (tests judged as
// production) rather than to a wrong answer.
func ensureTestNodesView(qg graph.RefsQuerier, hasAST bool) error {
	// Materialised as a TEMP TABLE, not a VIEW. The detection query walks a
	// 250k-row _ast twice (attribute lookup, then byte-range containment) and
	// costs ~2s on a mid-size Rust corpus. As a view every rule that filters
	// against it would re-run that; as a table the cost is paid once per
	// find-smells invocation and every rule joins a small indexed set.
	for _, s := range []string{
		// DROP TABLE only: `DROP VIEW IF EXISTS` ERRORS when the name exists as
		// a table ("use DROP TABLE to delete table"), and IF EXISTS does not
		// rescue a type mismatch. Only a table is ever created here.
		"DROP TABLE IF EXISTS temp.v_test_nodes",
		"CREATE TEMP TABLE v_test_nodes AS " + testNodesViewSQL(hasAST),
		"CREATE INDEX IF NOT EXISTS temp.idx_v_test_nodes ON v_test_nodes(node_id)",
	} {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure v_test_nodes: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}
