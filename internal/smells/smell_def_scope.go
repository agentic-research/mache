package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
)

// defScopeSQL builds `v_def_scope`: for every definition, each of its
// ancestors-or-self with how far up it is, its name and its AST kind.
//
// WHY. dead_code and duplicate_definitions skip whole categories of
// definition: anything under a mache-schema `types/` (or `imports/`,
// `constants/`, ...) directory, and anything that is, or sits inside, a
// tree-sitter construct such as a type_declaration or an impl_item. They used
// to say so by substring-matching node_id ("%type_declaration%",
// "%/types/%"), which reads a path into an id the vocabulary leaves opaque. On
// projection-v6 node_id is an integer and every such filter matched nothing
// (mache-2dc105). The question is about the node's ancestry, so this answers
// it from the ancestry: up v_nodes.parent_id, which both projections carry.
//
// depth is 0 for the definition itself. A category is a STRICT ancestor
// (depth > 0) named for it, as "%/types/%" required a segment after it; a kind
// counts at any depth, as "%type_declaration%" matched the node's own segment
// too.
//
// A TEMP TABLE, like v_test_nodes, so the walk runs once per find-smells
// invocation rather than once per rule that reads it.
func defScopeSQL(hasNodes, hasAST bool) string {
	if !hasNodes {
		return `SELECT '' AS node_id, 0 AS depth, '' AS name, '' AS kind WHERE 0`
	}
	kind, join := "NULL", ""
	if hasAST {
		kind, join = "a.node_kind", " LEFT JOIN v_ast a ON a.node_id = up.anc"
	}
	// The depth cap only guards a malformed table whose parent chain cycles;
	// real nesting is far shallower.
	return `
		WITH RECURSIVE up(node_id, anc, depth) AS (
			SELECT node_id, node_id, 0
			FROM (SELECT DISTINCT node_id FROM v_defs WHERE node_id IS NOT NULL)
			UNION ALL
			SELECT up.node_id, n.parent_id, up.depth + 1
			FROM up JOIN v_nodes n ON n.node_id = up.anc
			WHERE n.parent_id IS NOT NULL AND n.parent_id != '' AND up.depth < 256
		)
		SELECT up.node_id, up.depth, n.name, ` + kind + ` AS kind
		FROM up JOIN v_nodes n ON n.node_id = up.anc` + join
}

// ensureDefScopeTable installs v_def_scope on this connection.
func ensureDefScopeTable(qg graph.RefsQuerier, hasAST bool) error {
	hasNodes, err := lloschema.TableHasColumn(qg, "nodes", "kind")
	if err != nil {
		return fmt.Errorf("probe nodes: %w", err)
	}
	for _, s := range []string{
		"DROP TABLE IF EXISTS temp.v_def_scope",
		"CREATE TEMP TABLE v_def_scope AS " + defScopeSQL(hasNodes, hasAST),
		"CREATE INDEX IF NOT EXISTS temp.idx_v_def_scope ON v_def_scope(node_id)",
	} {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure v_def_scope: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}
