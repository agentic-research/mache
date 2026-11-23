package graph

import (
	"path/filepath"
	"strings"
)

// DefTokensFor returns the symbols a node defines.
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
// node_defs is the authority: it maps a node to the tokens that node declares,
// asked through the graph's DefTokener so this file names none of the
// producer's columns (projection-v6 renamed them, mache-90cb9f).
//
// The filepath.Base fallback is kept for the standalone mache schema (no
// node_defs table, or a backend with no SQL handle at all), where it IS the
// symbol — and only for that case.
func DefTokensFor(g Graph, nodeID string) []string {
	dt, ok := g.(DefTokener)
	if !ok {
		return []string{filepath.Base(nodeID)}
	}
	// A mount addresses nodes from the root ("/a.go/..."); ids do not.
	tokens, err := dt.DefTokensOf(strings.TrimPrefix(nodeID, "/"))
	if err != nil || len(tokens) == 0 {
		// No node_defs table (standalone projection), a query failure, or a
		// node that defines nothing (a directory, a file node, a virtual
		// node): filepath.Base is as good an answer as exists.
		return []string{filepath.Base(nodeID)}
	}
	return tokens
}

// CallersOf is what references the node at nodeID: the callers of every
// symbol it defines, each once, in token order then the graph's own. It is
// the one way a caller list is derived from a node, for MCP's list_directory,
// get_impact and get_dataflow and the mount's callers/, so they cannot
// disagree about who calls a node.
//
// follow, when not nil, says whether a token is looked up at all; get_impact
// uses it to leave ubiquitous names alone below the root.
func CallersOf(g Graph, nodeID string, follow func(token string) bool) ([]*Node, error) {
	var callers []*Node
	seen := map[string]bool{}
	for _, token := range DefTokensFor(g, nodeID) {
		if follow != nil && !follow(token) {
			continue
		}
		found, err := g.GetCallers(token)
		if err != nil {
			return nil, err
		}
		for _, c := range found {
			if c != nil && !seen[c.ID] {
				seen[c.ID] = true
				callers = append(callers, c)
			}
		}
	}
	return callers, nil
}
