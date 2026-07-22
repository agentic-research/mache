package graph

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// --- Import resolution for qualified callees ---

// loadImports returns alias → import-path mappings for a node.
//
// Imports are structured data produced at ingest (engine_walk.go marshals them
// into Properties["imports"]) and persisted into the nodes-table `record`
// column, which both SQLiteWriter.GetNode and NodesTableReader.GetNode restore.
// There is deliberately NO text-scraping fallback: a regex import parser used
// to sit here for "backward compatibility", but it was heuristical matching of
// Go source text that mis-classified dot-imports, and the paths that reached it
// are now covered structurally (mache-f930b6):
//
//   - MemoryStore (fresh ingest) — the engine sets Properties directly.
//   - .db built by SQLiteWriter — Properties round-trip via `record`.
//   - .db built by leyline — carries no `context` column at all, so the old
//     fallback could never have fired there anyway.
func loadImports(node *Node) map[string]string {
	if node.Properties == nil {
		return nil
	}
	raw, ok := node.Properties["imports"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var imports map[string]string
	if err := json.Unmarshal(raw, &imports); err != nil || len(imports) == 0 {
		return nil
	}
	return imports
}

// GetCallees implements Graph. It extracts calls made by the construct, then
// looks up those tokens in the defs index to find definitions.
//
// Three extraction paths are supported:
//
//  1. AST-scoped (preferred): when the construct's Properties carry
//     "ast_source_id"/"ast_scope_id" (persisted at projection time by
//     engine_walk.go — see ingest.ASTScope) AND a scoped extractor is
//     wired (requires a LIVE `_ast` table), GetCallees queries the
//     pre-parsed `_ast` table directly, scoped to this construct. This is
//     the fix for bead mache-fd9982: the legacy path below fed the GRAPH
//     node id (e.g. "cmd/functions/evalOrAbs") as if it were the `_ast`
//     source_id, which matched zero rows every time.
//  2. node_refs fallback: when #1 is unavailable but the construct has a
//     "source" file child, resolve its calls from s.refs (token ->
//     []nodeID) instead — the same index GetCallers reads, inverted.
//     s.refs already holds every call token ScopeCalls found for this
//     construct at projection time (engine_walk.go's
//     store.AddRef(token, sourceFileID)). Bare-token only (no qualifier
//     fidelity) — strictly better than returning [] (bead mache-6fbaf1).
//  3. Legacy content-based extractor: re-parses the construct's "source"
//     file content. Kept for non-AST backends (or .dbs projected before
//     this fix that lack the ast_source_id/ast_scope_id Properties AND
//     have no node_refs coverage).
func (s *MemoryStore) GetCallees(id string) ([]*Node, error) {
	s.mu.RLock()
	id = NormalizeID(id)
	node, ok := s.nodes[id]
	s.mu.RUnlock()

	if !ok || !node.Mode.IsDir() {
		return nil, nil
	}

	// Determine langName from construct directory Properties
	var langName string
	if node.Properties != nil {
		if v, ok := node.Properties["lang"]; ok {
			langName = string(v)
		}
	}

	var qcalls []QualifiedCall
	scoped := false
	if s.scopedExtractor != nil && node.Properties != nil {
		astSrcID, hasSrc := node.Properties["ast_source_id"]
		astScopeID, hasScope := node.Properties["ast_scope_id"]
		if hasSrc && hasScope && len(astSrcID) > 0 && len(astScopeID) > 0 {
			calls, err := s.scopedExtractor(string(astSrcID), string(astScopeID), langName)
			if err != nil {
				return nil, fmt.Errorf("extract calls (ast-scoped): %w", err)
			}
			qcalls = calls
			scoped = true
		}
	}

	if !scoped {
		// 1. Find the "source" file child
		var sourceID string
		for _, childID := range node.Children {
			if filepath.Base(childID) == "source" {
				sourceID = childID
				break
			}
		}
		if sourceID == "" {
			return nil, nil
		}

		// node_refs fallback (bead mache-6fbaf1): mirrors
		// SQLiteGraph.calleeTokensFromRefs for backend parity. s.refs
		// (token -> []nodeID) already holds every call token ScopeCalls
		// found for this construct at projection time, keyed by the
		// construct's "source" file node id — resolve straight from
		// there instead of re-reading and re-parsing content that (on a
		// backend with no live scoped extractor) no extractor can read.
		if tokens := s.refTokensForNode(sourceID); len(tokens) > 0 {
			for _, tok := range tokens {
				qcalls = append(qcalls, QualifiedCall{Token: tok})
			}
		}

		if len(qcalls) == 0 {
			// 2. Read content
			srcNode, err := s.GetNode(sourceID)
			if err != nil {
				return nil, err
			}
			size := srcNode.ContentSize()
			buf := make([]byte, size)
			if _, err := s.ReadContent(sourceID, buf, 0); err != nil {
				return nil, err
			}

			// 3. Extract qualified calls
			if s.extractor == nil {
				return nil, nil
			}
			calls, err := s.extractor(buf, sourceID, langName)
			if err != nil {
				return nil, fmt.Errorf("extract calls: %w", err)
			}
			qcalls = calls
		}
	}

	// 5. Resolve tokens via defs index (qualified → import fallback → bare)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*Node
	seen := make(map[string]bool)
	var imports map[string]string // lazy-parsed Go imports

	for _, qc := range qcalls {
		resolved := false

		// Qualified resolution: "auth.Validate" → defs["auth.Validate"]
		if qc.Qualifier != "" {
			qualKey := qc.Qualifier + "." + qc.Token
			if defIDs, ok := s.defs[qualKey]; ok {
				for _, defID := range defIDs {
					if defID == id || seen[defID] {
						continue
					}
					if defNode, ok := s.nodes[defID]; ok {
						results = append(results, defNode)
						seen[defID] = true
						resolved = true
					}
				}
			}

			// Import-path fallback for aliased imports:
			// import mypkg "github.com/foo/bar/auth" → mypkg.Validate → auth.Validate
			if !resolved && (node.Context != nil || node.Properties != nil) {
				if imports == nil {
					imports = loadImports(node)
				}
				if importPath, ok := imports[qc.Qualifier]; ok {
					altPkg := filepath.Base(importPath)
					altKey := altPkg + "." + qc.Token
					if defIDs, ok := s.defs[altKey]; ok {
						for _, defID := range defIDs {
							if defID == id || seen[defID] {
								continue
							}
							if defNode, ok := s.nodes[defID]; ok {
								results = append(results, defNode)
								seen[defID] = true
								resolved = true
							}
						}
					}
				}
			}

			if resolved {
				continue
			}
		}

		// Bare token lookup (unqualified calls or failed qualified resolution)
		if defIDs, ok := s.defs[qc.Token]; ok {
			for _, defID := range defIDs {
				if defID == id || seen[defID] {
					continue
				}
				if defNode, ok := s.nodes[defID]; ok {
					results = append(results, defNode)
					seen[defID] = true
				}
			}
		}
	}

	return results, nil
}

// refTokensForNode returns the call tokens s.refs recorded for sourceID (a
// construct's "source" file node id) — the reverse of s.refs's
// token->[]nodeID mapping (which GetCallers reads forward: token -> who
// calls it). This is the MemoryStore counterpart of
// SQLiteGraph.calleeTokensFromRefs — same node_refs semantics, in-memory
// index instead of a SQL table (bead mache-6fbaf1). O(total refs): there is
// no reverse index maintained at write time, so this scans every token's
// id list. Acceptable here since it only runs as a fallback when the
// scoped-AST path is unavailable.
func (s *MemoryStore) refTokensForNode(sourceID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var tokens []string
	for token, ids := range s.refs {
		for _, refID := range ids {
			if refID == sourceID {
				tokens = append(tokens, token)
				break
			}
		}
	}
	return tokens
}
