package cmd

import (
	"path/filepath"

	"github.com/agentic-research/mache/graph"
)

// tokenForNode returns the symbol a node defines.
//
// Callers that need "what is this node called?" must NOT reach for
// filepath.Base(nodeID). That works only on the standalone mache schema, where
// a node ID ends in the symbol. On a leyline projection — mache's primary
// backend — IDs end in a tree-sitter construct kind plus an ordinal:
//
//	a.go/function_declaration_0    not    a.go/Alpha
//	pkg/type_spec                  not    pkg/Greeter
//
// Measured over a real projection of this repo: of 10,409 rows in node_defs,
// ZERO have a node_id ending in their own token. So the path-segment guess is
// not merely lossy there, it is never right (mache-cb4369).
//
// That mattered because it did not fail closed. GetCallers("function_declaration")
// matches that literal string wherever it appears, and leyline indexes markdown
// too — so get_impact answered with code spans from docs/ARCHITECTURE.md
// presented as call sites. Wrong answers, not empty ones.
//
// node_defs is the authority: it maps node_id to the token that node declares.
// The filepath.Base fallback is kept for the standalone mache schema (no
// node_defs table, or a backend with no SQL handle at all), where it IS the
// symbol — and only for that case.
func tokenForNode(g graph.Graph, nodeID string) string {
	qg, ok := g.(graph.RefsQuerier)
	if !ok {
		return filepath.Base(nodeID)
	}
	rows, err := qg.QueryRefs("SELECT token FROM node_defs WHERE node_id = ? LIMIT 1", nodeID)
	if err != nil {
		// No node_defs table (standalone projection) or a query failure: fall
		// back rather than losing the traversal entirely.
		return filepath.Base(nodeID)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var token string
		if err := rows.Scan(&token); err == nil && token != "" {
			return token
		}
	}
	// A node with no node_defs row is not a definition — a directory, a file
	// node, a virtual node. filepath.Base is as good an answer as exists.
	return filepath.Base(nodeID)
}
