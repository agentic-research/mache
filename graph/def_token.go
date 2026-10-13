package graph

import (
	"path/filepath"
	"strings"
)

// DefTokenFor returns the symbol a node defines.
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
// node_defs is the authority: it maps a node to the token that node declares,
// asked through the graph's DefTokener so this file names none of the
// producer's columns (projection-v6 renamed them, mache-90cb9f).
//
// Every surface that names a construct's callers asks this: MCP's
// list_directory, get_impact and get_dataflow, and the mount's callers/.
// The filepath.Base fallback is kept for the standalone mache schema (no
// node_defs table, or a backend with no SQL handle at all), where it IS the
// symbol — and only for that case.
func DefTokenFor(g Graph, nodeID string) string {
	dt, ok := g.(DefTokener)
	if !ok {
		return filepath.Base(nodeID)
	}
	// A mount addresses nodes from the root ("/a.go/..."); ids do not.
	token, ok, err := dt.DefTokenOf(strings.TrimPrefix(nodeID, "/"))
	if err != nil || !ok {
		// No node_defs table (standalone projection), a query failure, or a
		// node that defines nothing (a directory, a file node, a virtual
		// node): filepath.Base is as good an answer as exists.
		return filepath.Base(nodeID)
	}
	return token
}
