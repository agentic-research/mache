package graph

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// --- Import parsing for qualified callees resolution ---

var (
	singleImportRe = regexp.MustCompile(`import\s+(\w+\s+)?"([^"]+)"`)
	groupImportRe  = regexp.MustCompile(`(?s)import\s*\(([^)]*)\)`)
	memberImportRe = regexp.MustCompile(`(\w+)?\s*"([^"]+)"`)
)

// loadImports returns structured import mappings from a node.
// Prefers Properties["imports"] (JSON, set by tree-sitter during ingestion).
// Falls back to regex parsing of Context text for backward compatibility.
func loadImports(node *Node) map[string]string {
	if node.Properties != nil {
		if raw, ok := node.Properties["imports"]; ok && len(raw) > 0 {
			var imports map[string]string
			if err := json.Unmarshal(raw, &imports); err == nil && len(imports) > 0 {
				return imports
			}
		}
	}
	if node.Context != nil {
		return parseGoImports(node.Context)
	}
	return nil
}

// parseGoImports extracts alias → import path mappings from Go context text.
// For unaliased imports, the alias is the last path segment.
// Deprecated: prefer structured imports from Properties["imports"].
func parseGoImports(ctx []byte) map[string]string {
	imports := make(map[string]string)
	text := string(ctx)

	for _, m := range singleImportRe.FindAllStringSubmatch(text, -1) {
		addGoImport(imports, strings.TrimSpace(m[1]), m[2])
	}

	for _, m := range groupImportRe.FindAllStringSubmatch(text, -1) {
		for _, im := range memberImportRe.FindAllStringSubmatch(m[1], -1) {
			addGoImport(imports, strings.TrimSpace(im[1]), im[2])
		}
	}

	return imports
}

func addGoImport(imports map[string]string, alias, path string) {
	if alias == "_" || alias == "." {
		return
	}
	if alias == "" {
		alias = filepath.Base(path)
	}
	imports[alias] = path
}

// GetCallees implements Graph. It extracts calls made by the construct, then
// looks up those tokens in the defs index to find definitions.
//
// Two extraction paths are supported:
//
//  1. AST-scoped (preferred): when the construct's Properties carry
//     "ast_source_id"/"ast_scope_id" (persisted at projection time by
//     engine_walk.go — see ingest.ASTScope), GetCallees queries the
//     pre-parsed `_ast` table directly, scoped to this construct. This is
//     the fix for bead mache-fd9982: the legacy path below fed the GRAPH
//     node id (e.g. "cmd/functions/evalOrAbs") as if it were the `_ast`
//     source_id, which matched zero rows every time.
//  2. Legacy content-based extractor: re-parses the construct's "source"
//     file content. Kept for non-AST backends (or .dbs projected before
//     this fix that lack the ast_source_id/ast_scope_id Properties).
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
