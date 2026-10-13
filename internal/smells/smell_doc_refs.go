package smells

import (
	"fmt"

	"github.com/agentic-research/mache/internal/lloschema"

	"github.com/agentic-research/mache/graph"
)

// docRefsViewSQL builds `v_doc_refs` — markdown backtick-span citations,
// scoped to Rust paths, with the noise this measurably needed filtered out
// before any consumer sees a row. Used by drift_doc_dead_symbol_reference.
//
// WHY THE VOCABULARY AND NOT node_refs. The file a reference sits in is
// node_refs.source_id on projection-v4, absent from mache's own slimmer
// node_refs, and dropped by projection-v6, where the site's nid names its file.
// v_refs.source_id answers it for all three (NULL where no file is known), so
// this reads v_refs and a database with no file for its references simply
// yields no candidates (mache-2dc105).
//
// WHY RUST-PATH SCOPED. See drift_doc_dead_symbol_reference.json's
// Description for the full reasoning: ley-line-open-651909 (Go package-level
// consts emit no defs) is open, and Go has no '::' syntax, so scoping to
// tokens containing '::' sidesteps that gap entirely. The exclusions below
// (paths, whitespace, braces) were measured directly against mache's own
// docs — a naive join without them flagged .db, [], bead IDs, model names,
// and file paths as "dead symbols."
func docRefsViewSQL(hasRefs bool) string {
	if !hasRefs {
		// No node_refs at all: v_refs names a table that is not there, and
		// there is nothing to cite. An empty view keeps the consuming rule's
		// SQL identical rather than branching on schema shape.
		return `SELECT '' AS token, '' AS node_id, '' AS source_id, '' AS bare WHERE 0`
	}
	return `
		WITH candidate AS (
			SELECT DISTINCT nr.token, nr.node_id, nr.source_id
			FROM v_refs nr
			WHERE nr.fidelity = 'mention'
			  AND nr.source_id LIKE '%.md'
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
func ensureDocRefsView(qg graph.RefsQuerier) error {
	hasRefs, err := lloschema.TableHasColumn(qg, "node_refs", "token")
	if err != nil {
		return fmt.Errorf("probe node_refs: %w", err)
	}
	for _, s := range []string{
		"DROP TABLE IF EXISTS temp.v_doc_refs",
		"CREATE TEMP TABLE v_doc_refs AS " + docRefsViewSQL(hasRefs),
	} {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure v_doc_refs: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}
