package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
)

// Known construct kinds mache projects today. The path layout (per
// cmd/agent.go's user-facing tour) puts construct directories at
// `{pkg}/<kind-plural>/<symbol>/...` — `functions/`, `methods/`,
// `types/`, `constants/`, `variables/`, `imports/`. The MCP API
// accepts the singular form (matching LSP-style kind names) and we
// translate to the plural directory segment here.
var kindToPathSegment = map[string]string{
	"function": "/functions/",
	"method":   "/methods/",
	"type":     "/types/",
	"constant": "/constants/",
	"variable": "/variables/",
	"import":   "/imports/",
}

// supportedKinds returns the singular kind names accepted by mache's
// MCP tools. The slice is sorted so error messages and tool
// documentation produce stable output (map iteration is otherwise
// nondeterministic — flagged by Copilot review on PR #438).
func supportedKinds() []string {
	out := make([]string, 0, len(kindToPathSegment))
	for k := range kindToPathSegment {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// filterDirIDsByKind keeps only dirIDs whose construct path segment
// matches the requested kind. An empty `kind` is the no-op identity —
// caller is responsible for not invoking this on inputs they expect
// to be returned verbatim. An unknown `kind` returns nil and `false`
// so the handler can surface a precise error rather than silently
// returning empty results.
func filterDirIDsByKind(dirIDs []string, kind string) ([]string, bool) {
	if kind == "" {
		return dirIDs, true
	}
	segment, ok := kindToPathSegment[kind]
	if !ok {
		return nil, false
	}
	filtered := make([]string, 0, len(dirIDs))
	for _, id := range dirIDs {
		if strings.Contains(id, segment) {
			filtered = append(filtered, id)
		}
	}
	return filtered, true
}

// astNodeKindToCanonical maps tree-sitter node-kind names (as emitted
// by ley-line into _ast.node_kind) to mache's singular canonical kinds
// (the keys of kindToPathSegment). This is the projection-independent
// kind source: leyline paths carry no `/functions/` segment, so kind
// must be resolved from node_kind, not the path string (bead mache-5bb181).
//
// `function_item` (Rust) and `function_definition` (Python) are
// AMBIGUOUS — they are the node kind for BOTH free functions and
// methods, distinguished only by ancestry. They map to "function" here;
// resolveKindFromAST upgrades them to "method" when a method-container
// ancestor (impl_item / trait_item / class) is present.
var astNodeKindToCanonical = map[string]string{
	// Go
	"function_declaration": "function",
	"method_declaration":   "method",
	"type_declaration":     "type",
	"type_spec":            "type",
	"const_spec":           "constant",
	"var_spec":             "variable",
	"import_spec":          "import",
	// Rust (function_item = function OR method — ancestry decides)
	"function_item":           "function",
	"function_signature_item": "method", // trait method signature (no body) — always a method
	"struct_item":             "type",
	"enum_item":               "type",
	"trait_item":              "type",
	"type_item":               "type",
	"union_item":              "type",
	"const_item":              "constant",
	"static_item":             "constant",
	"let_declaration":         "variable",
	"use_declaration":         "import",
	// Python (function_definition = function OR method — ancestry decides)
	"function_definition": "function",
	"class_definition":    "type",
	// JavaScript / TypeScript
	"method_definition": "method",
	"class_declaration": "type",
	"import_statement":  "import",
}

// hasASTTable reports whether the backing SQL store exposes an `_ast`
// table (i.e. it is a ley-line-parsed projection). Construct-dir
// MemoryStore mounts have no `_ast` and fall back to path-segment matching.
func hasASTTable(qg refsQuerier) bool {
	rows, err := qg.QueryRefs(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='_ast' LIMIT 1`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	return rows.Next()
}

// resolveKindFromAST resolves a node's canonical kind from _ast.node_kind,
// upgrading ambiguous function kinds to "method" when the node has a
// method-container ancestor. Returns "" when the node is absent from
// _ast or its node_kind is unmapped — caller falls back to path-segment.
func resolveKindFromAST(qg refsQuerier, nodeID string) string {
	rows, err := qg.QueryRefs(`SELECT node_kind FROM _ast WHERE node_id = ? LIMIT 1`, nodeID)
	if err != nil {
		return ""
	}
	var nodeKind string
	func() {
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			_ = rows.Scan(&nodeKind)
		}
	}()
	canonical, ok := astNodeKindToCanonical[nodeKind]
	if !ok {
		return ""
	}
	if canonical == "function" && astNodeHasMethodContainerAncestor(qg, nodeID) {
		return "method"
	}
	return canonical
}

// astNodeHasMethodContainerAncestor walks the nodes.parent_id chain and
// reports whether any ancestor's _ast.node_kind is a method container.
// A single recursive CTE keeps this one round-trip regardless of depth.
func astNodeHasMethodContainerAncestor(qg refsQuerier, nodeID string) bool {
	// The depth column bounds recursion at 256 — real AST ancestry to a
	// method container is shallow; the cap is purely a guard against a
	// malformed leyline `nodes` table with a cyclic parent_id (which would
	// otherwise spin forever). No legitimate source nests this deep.
	rows, err := qg.QueryRefs(`
		WITH RECURSIVE ancestors(id, depth) AS (
			SELECT parent_id, 0 FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.parent_id, a.depth + 1 FROM nodes n JOIN ancestors a ON n.id = a.id
			WHERE n.parent_id IS NOT NULL AND n.parent_id != '' AND a.depth < 256
		)
		SELECT 1 FROM ancestors a JOIN _ast x ON x.node_id = a.id
		WHERE x.node_kind IN ('impl_item','trait_item','class_definition','class_declaration')
		LIMIT 1`, nodeID)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	return rows.Next()
}

// filterDirIDsByKindGraph is the projection-aware kind filter. When the
// graph is a ley-line projection (has an `_ast` table), it resolves each
// candidate's kind from _ast.node_kind (+ ancestry) — the load-bearing
// path. Otherwise (construct-dir MemoryStore, no _ast) it falls back to
// filterDirIDsByKind's path-segment match — the documented TRANSITIONAL
// fallback that goes away when SitterWalker is deleted and every backend
// has _ast. A node_id absent from _ast also degrades to path-segment so
// mixed/partial projections never silently drop hits (bead mache-5bb181).
func filterDirIDsByKindGraph(g graph.Graph, dirIDs []string, kind string) ([]string, bool) {
	if kind == "" {
		return dirIDs, true
	}
	segment, known := kindToPathSegment[kind]
	if !known {
		return nil, false
	}
	qg, ok := g.(refsQuerier)
	if !ok || !hasASTTable(qg) {
		return filterDirIDsByKind(dirIDs, kind)
	}
	filtered := make([]string, 0, len(dirIDs))
	for _, id := range dirIDs {
		switch resolveKindFromAST(qg, id) {
		case kind:
			filtered = append(filtered, id)
		case "":
			// Not in _ast — fall back to path-segment for this id.
			if strings.Contains(id, segment) {
				filtered = append(filtered, id)
			}
		}
	}
	return filtered, true
}

// validateKindParam pulls the optional `kind` param from an MCP
// request and validates it against the supported kind set. Returns
// either (kind, nil) on success or ("", errResult) when the kind
// is unknown. Caller propagates errResult and returns immediately
// when non-nil. Extracted from per-handler boilerplate to keep
// each handler's fan-out under the find_smells threshold (Copilot
// review on PR #438, fan_out_skew advisory).
func validateKindParam(request mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	kind := request.GetString("kind", "")
	if _, ok := filterDirIDsByKind(nil, kind); !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"unknown kind %q — accepted values: %s",
			kind, strings.Join(supportedKinds(), ", ")))
	}
	return kind, nil
}

// filterByNodeIDKind keeps only items whose NodeID (extracted via
// idOf) matches the requested kind. Generic over the item type
// because lspDefLocation, lspRefLocation, and hover/diag result
// shapes all carry a NodeID field but have distinct struct types.
// Returns items unchanged when kind == "". Extracted from the
// repeated build-IDs / build-keep-map / filter-results loop that
// appeared 4 times across serve_lsp.go + serve_handler_*.go
// (Copilot fan_out_skew advisory).
func filterByNodeIDKind[T any](items []T, kind string, idOf func(T) string) []T {
	if kind == "" {
		return items
	}
	ids := make([]string, len(items))
	for i, x := range items {
		ids[i] = idOf(x)
	}
	filteredIDs, _ := filterDirIDsByKind(ids, kind)
	keep := make(map[string]bool, len(filteredIDs))
	for _, id := range filteredIDs {
		keep[id] = true
	}
	kept := items[:0]
	for _, x := range items {
		if keep[idOf(x)] {
			kept = append(kept, x)
		}
	}
	return kept
}
