package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
)

// docRefsViewSQL builds `v_doc_refs` — markdown backtick-span citations,
// scoped to Rust paths, with the noise this measurably needed filtered out
// before any consumer sees a row. Used by drift_doc_dead_symbol_reference.
//
// WHY A VIEW AND NOT A RAW node_refs QUERY. node_refs carries a source_id
// column on every genuine leyline-native .db (populated for both source
// refs and markdown code-span refs alike), but mache's own schema-projection
// output builds a slimmer node_refs with only (token, node_id) — and so do
// many hand-authored test fixtures that only need node_refs for an unrelated
// rule. A query that references nr.source_id directly fails to PREPARE (not
// just to match rows) against that shape: SQLite checks column existence at
// bind time, not filtered by any runtime WHERE condition, so there is no SQL
// idiom that makes the reference conditional within a single static query.
// Probing the column in Go first — exactly how hasRefsContainer /
// hasRefsQualifier / hasDefsCanonicalKind already handle the same class of
// schema variability in EnsureCanonicalViews — and emitting the degrade-to-
// empty form when it's absent is the only way to keep this optional rather
// than fatal.
//
// WHY RUST-PATH SCOPED. See drift_doc_dead_symbol_reference.json's
// Description for the full reasoning: ley-line-open-651909 (Go package-level
// consts emit no defs) is open, and Go has no '::' syntax, so scoping to
// tokens containing '::' sidesteps that gap entirely. The exclusions below
// (paths, whitespace, braces) were measured directly against mache's own
// docs — a naive join without them flagged .db, [], bead IDs, model names,
// and file paths as "dead symbols."
func docRefsViewSQL(hasSourceID bool) string {
	if !hasSourceID {
		// No source_id column: nothing to scope by file, so nothing is a
		// candidate. An empty view keeps the consuming rule's SQL
		// identical rather than branching on schema shape.
		return `SELECT '' AS token, '' AS node_id, '' AS source_id, '' AS bare WHERE 0`
	}
	return `
		WITH candidate AS (
			SELECT DISTINCT nr.token, nr.node_id, nr.source_id
			FROM node_refs nr
			WHERE nr.source_id LIKE '%.md'
			  AND nr.token LIKE '%::%'
			  AND nr.token NOT LIKE '%/%'
			  AND nr.token NOT LIKE '% %'
			  AND nr.token NOT LIKE '%' || char(10) || '%'
			  AND nr.token NOT LIKE '%{%'
			  AND nr.token NOT LIKE '%}%'
		)
		-- bare: the token with a trailing call-syntax '()' stripped, so
		-- 'Stdio::null()' can match a def token of 'null' the same way
		-- 'Stdio::null' would.
		SELECT token, node_id, source_id,
		       CASE WHEN token LIKE '%()' THEN substr(token, 1, length(token) - 2) ELSE token END AS bare
		FROM candidate`
}

// ensureDocRefsView installs v_doc_refs. Best-effort in the same sense as
// the other canonical views: a failure to build it must not fail the run.
func ensureDocRefsView(qg graph.RefsQuerier, hasSourceID bool) error {
	for _, s := range []string{
		"DROP TABLE IF EXISTS temp.v_doc_refs",
		"CREATE TEMP TABLE v_doc_refs AS " + docRefsViewSQL(hasSourceID),
	} {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure v_doc_refs: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}
