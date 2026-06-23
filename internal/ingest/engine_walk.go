package ingest

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
)

// toNodeID converts a filesystem path to a graph node ID by normalizing
// separators and stripping the leading slash.
func toNodeID(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "/")
}

// dedupSuffix returns a ".from_<sanitized>" suffix derived from the source filename.
// Dots in the filename are replaced with underscores to avoid path separator confusion.
// e.g., "a.go" -> ".from_a_go"
func dedupSuffix(sourceFile string) string {
	sanitized := strings.ReplaceAll(sourceFile, ".", "_")
	return ".from_" + sanitized
}

// processRecord is a pure function — parses one SQLite record through the schema
// and returns all nodes to create, without touching the store.
//
// extraFuncs and tmplCache enable template functions like {{diagram}} in content
// templates. When non-nil, content rendering uses RenderTemplateWithFuncs with
// these extras merged in. When nil, falls back to RenderTemplate (base funcs only).
func processRecord(schema *api.Topology, walker Walker, dbPath string, job recordJob, extraFuncs template.FuncMap, tmplCache *sync.Map) recordResult {
	var parsed any
	if err := json.Unmarshal([]byte(job.raw), &parsed); err != nil {
		return recordResult{err: fmt.Errorf("parse record %s: %w", job.recordID, err)} // coverage:ignore
	} // coverage:ignore

	wrapper := []any{parsed}
	var result recordResult

	// Expose the full record as _parent context for child selectors.
	// In the streaming path, each record IS the top-level match — its fields
	// should be accessible via {{._parent.item.Advisory.Severity}} etc.
	recordValues, _ := parsed.(map[string]any)

	for _, nodeSchema := range schema.Nodes {
		for _, childSchema := range nodeSchema.Children {
			collectNodes(&result, childSchema, walker, wrapper, nodeSchema.Name, dbPath, job.recordID, extraFuncs, tmplCache, recordValues)
			if result.err != nil {
				return result // coverage:ignore
			} // coverage:ignore
		}
	}

	return result
}

// collectNodes is the pure equivalent of processNode — builds node lists
// without any store access. Safe to call from multiple goroutines.
//
// extraFuncs/tmplCache are threaded through for content template rendering
// (e.g., {{diagram}}). When nil, uses base RenderTemplate.
func collectNodes(result *recordResult, schema api.Node, walker Walker, ctx any, parentPath, dbPath, recordID string, extraFuncs template.FuncMap, tmplCache *sync.Map, parentMatchValues map[string]any) {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		result.err = fmt.Errorf("query failed for %s: %w", schema.Name, err) // coverage:ignore
		return                                                               // coverage:ignore
	} // coverage:ignore

	// Inject _parent context into child matches when parent values are available.
	if parentMatchValues != nil {
		for i, m := range matches {
			matches[i] = &parentAwareMatch{inner: m, parentValues: parentMatchValues}
		}
	}

	for _, match := range matches {
		name, err := RenderTemplate(schema.Name, match.Values())
		if err != nil {
			// Skip records whose structure doesn't match this schema node.
			// This allows a single schema to handle mixed-format data sources
			// (e.g. vunnel OS format + OSV format in the same results table).
			log.Printf("[WARN] skipping record: failed to render name %s: %v", schema.Name, err) // coverage:ignore
			continue                                                                             // coverage:ignore
		}

		currentPath := filepath.Join(parentPath, name)
		id := toNodeID(currentPath)

		node := &graph.Node{
			ID:      id,
			Mode:    os.ModeDir | 0o555,
			ModTime: time.Unix(0, 0),
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				collectNodes(result, childSchema, walker, nextCtx, currentPath, dbPath, recordID, extraFuncs, tmplCache, match.Values())
				if result.err != nil {
					return // coverage:ignore
				} // coverage:ignore
			}
		}

		// Process files
		for _, fileSchema := range schema.Files {
			fileName, err := RenderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				log.Printf("collectNodes: skip file name render %q: %v", fileSchema.Name, err) // coverage:ignore
				continue                                                                       // coverage:ignore
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := toNodeID(filePath)

			var content string
			if len(extraFuncs) > 0 && tmplCache != nil {
				content, err = RenderTemplateWithFuncs(fileSchema.ContentTemplate, match.Values(), extraFuncs, tmplCache)
			} else {
				content, err = RenderTemplate(fileSchema.ContentTemplate, match.Values()) // coverage:ignore
			} // coverage:ignore
			if err != nil {
				log.Printf("collectNodes: skip file content render %q: %v", fileId, err) // coverage:ignore
				continue                                                                 // coverage:ignore
			}

			fileNode := &graph.Node{
				ID:      fileId,
				Mode:    0o444,
				ModTime: time.Unix(0, 0),
			}

			// Inline small content, lazy-resolve large content from SQLite
			if len(content) > inlineThreshold {
				fileNode.Ref = &graph.ContentRef{ // coverage:ignore
					DBPath:     dbPath,                     // coverage:ignore
					RecordID:   recordID,                   // coverage:ignore
					Template:   fileSchema.ContentTemplate, // coverage:ignore
					ContentLen: int64(len(content)),        // coverage:ignore
				} // coverage:ignore
			} else {
				fileNode.Data = []byte(content)
			}

			result.nodes = append(result.nodes, fileNode)
			node.Children = append(node.Children, fileId)
		}

		result.nodes = append(result.nodes, node)

		// Collect schema-declared refs (cross-reference tokens for callers/)
		for _, refTmpl := range schema.Refs {
			token, err := RenderTemplate(refTmpl, match.Values())
			if err != nil {
				result.err = fmt.Errorf("failed to render ref %s: %w", refTmpl, err) // coverage:ignore
				return                                                               // coverage:ignore
			} // coverage:ignore
			if token != "" {
				result.refLinks = append(result.refLinks, refLink{token: token, nodeID: id})
			}
		}

		// Link to parent (collector will apply this)
		parentID := toNodeID(parentPath)
		result.parentLinks = append(result.parentLinks, parentLink{childID: id, parentID: parentID})
	}
}

// processNode is the schema-driven recursion that walks a single Match through
// the topology, mutating the store as it goes. Counterpart to collectNodes,
// which is the pure (store-free) variant for parallel SQLite ingest.
func (e *Engine) processNode(schema api.Node, walker Walker, ctx any, parentPath, sourceFile, absSourceFile string, modTime time.Time, store IngestionTarget, fileContext []byte, fileAddressRefs []string, parentMatchValues map[string]any, fileImports map[string]string) error {
	matches, err := walker.Query(ctx, schema.Selector)
	if err != nil {
		return fmt.Errorf("query failed for %s: %w", schema.Name, err)
	}

	// Inject _parent context into child matches when parent values are available.
	if parentMatchValues != nil {
		for i, m := range matches {
			matches[i] = &parentAwareMatch{inner: m, parentValues: parentMatchValues}
		}
	}

	for _, match := range matches {
		// Skip self-match if requested (e.g. for recursive schemas to avoid infinite loops)
		if schema.SkipSelfMatch {
			// Check for Tree-sitter node equality using byte ranges
			if parentRoot, ok := ctx.(SitterRoot); ok { // coverage:ignore
				if childCtx, ok := match.Context().(SitterRoot); ok { // coverage:ignore
					if parentRoot.Node.StartByte() == childCtx.Node.StartByte() && // coverage:ignore
						parentRoot.Node.EndByte() == childCtx.Node.EndByte() && // coverage:ignore
						parentRoot.Node.Type() == childCtx.Node.Type() { // coverage:ignore
						continue // coverage:ignore
					}
				}
			}
		}

		name, err := RenderTemplate(schema.Name, match.Values())
		if err != nil {
			log.Printf("[WARN] skipping file: failed to render name %s: %v", schema.Name, err) // coverage:ignore
			continue                                                                           // coverage:ignore
		}

		// Normalize path
		currentPath := filepath.Join(parentPath, name)
		id := toNodeID(currentPath)

		// Dedup: when this node has files and a node with the same ID
		// already exists with file children (i.e., from a different source file),
		// append a source-file suffix to disambiguate.
		// This handles cases like multiple init() functions across Go files.
		if len(schema.Files) > 0 && sourceFile != "" {
			if existing, err := store.GetNode(id); err == nil && len(existing.Children) > 0 {
				suffix := dedupSuffix(sourceFile)
				name = name + suffix
				currentPath = filepath.Join(parentPath, name)
				id = toNodeID(currentPath)
			}
		}

		// Create/Update Node — preserve existing children when merging
		// multiple files into the same node (e.g. multiple .go files in one package).
		var existingChildren []string
		if existing, err := store.GetNode(id); err == nil {
			existingChildren = existing.Children
		}

		node := &graph.Node{
			ID:       id,
			Mode:     os.ModeDir | 0o555, // Read-only dir
			ModTime:  modTime,            // Propagate source file time
			Children: existingChildren,
		}

		// Store language + package name as node properties (used for callees/
		// qualified-def resolution). Read through the FileMeta interface so the
		// engine stays walker-agnostic — both sitterMatch and astMatch implement
		// it (and parentAwareMatch forwards to its inner), so this no longer
		// type-switches on the concrete walker. Parity between the two backends is
		// asserted by TestASTQueryParity; correctness of the values by
		// TestEngine_MethodReceiverShape_RegistersBareLeafDef.
		if fm, ok := match.(FileMeta); ok {
			if l := fm.Lang(); l != "" {
				if node.Properties == nil {
					node.Properties = make(map[string][]byte)
				}
				node.Properties["lang"] = []byte(l)

				// Go package name, for qualified def resolution.
				if pkg := fm.PackageName(); pkg != "" {
					node.Properties["pkg"] = []byte(pkg)
				}
			}
		}

		// Store structured imports (avoids regex re-parsing at query time).
		// Independent of walker type — persist whenever fileImports is non-nil.
		if fileImports != nil {
			if node.Properties == nil {
				node.Properties = make(map[string][]byte) // coverage:ignore
			} // coverage:ignore
			if importJSON, err := json.Marshal(fileImports); err == nil {
				node.Properties["imports"] = importJSON
			}
		}
		store.AddNode(node)

		// Register definition: construct name → directory ID
		if len(schema.Files) > 0 {
			if err := store.AddDef(name, id); err != nil {
				return fmt.Errorf("add def %s -> %s: %w", name, id, err) // coverage:ignore
			} // coverage:ignore
			// Register qualified definition (package.name → directory ID)
			if node.Properties != nil {
				if pkg, ok := node.Properties["pkg"]; ok && len(pkg) > 0 {
					qualKey := string(pkg) + "." + name
					if err := store.AddDef(qualKey, id); err != nil {
						return fmt.Errorf("add qualified def %s -> %s: %w", qualKey, id, err) // coverage:ignore
					} // coverage:ignore
				}
			}
			// When the schema renders a Receiver.Method shape (the
			// go-schema methods/ branch uses '{{.receiver}}.{{.name}}'),
			// also register the bare leaf token so call-extraction —
			// which captures the field_identifier of obj.Method() as
			// just 'Method' — can resolve to this def. Without this,
			// every method on a typed receiver looks dead in dead_code
			// because 'Method' (call) doesn't match 'Receiver.Method'
			// (def). We don't strip on every dot — only the last one,
			// since fully-qualified shapes like 'pkg.Receiver.Method'
			// are added separately above.
			if dot := strings.LastIndex(name, "."); dot > 0 && dot < len(name)-1 {
				leaf := name[dot+1:]
				if err := store.AddDef(leaf, id); err != nil {
					return fmt.Errorf("add bare leaf def %s -> %s: %w", leaf, id, err) // coverage:ignore
				} // coverage:ignore
			}
		}

		// Register schema-declared refs (cross-reference tokens for callers/)
		for _, refTmpl := range schema.Refs {
			token, err := RenderTemplate(refTmpl, match.Values()) // coverage:ignore
			if err != nil {                                       // coverage:ignore
				return fmt.Errorf("failed to render ref %s: %w", refTmpl, err) // coverage:ignore
			} // coverage:ignore
			if token != "" { // coverage:ignore
				if err := store.AddRef(token, id); err != nil { // coverage:ignore
					return fmt.Errorf("add ref %s -> %s: %w", token, id, err) // coverage:ignore
				} // coverage:ignore
			}
		}

		// Link to parent
		if parentPath == "" {
			store.AddRoot(node)
		} else {
			parentId := toNodeID(parentPath)
			parent, err := store.GetNode(parentId)
			if err == nil {
				if e.childSeen[parentId] == nil {
					e.childSeen[parentId] = make(map[string]bool, len(parent.Children))
					for _, c := range parent.Children {
						e.childSeen[parentId][c] = true
					}
				}
				if !e.childSeen[parentId][id] {
					e.childSeen[parentId][id] = true
					parent.Children = append(parent.Children, id)
					store.AddNode(parent)
				}
			}
		}

		// Recurse children
		nextCtx := match.Context()
		if nextCtx != nil {
			for _, childSchema := range schema.Children {
				if err := e.processNode(childSchema, walker, nextCtx, currentPath, sourceFile, absSourceFile, modTime, store, fileContext, fileAddressRefs, match.Values(), fileImports); err != nil {
					return err // coverage:ignore
				} // coverage:ignore
			}
		}

		// Extract calls for this match (refs index)
		var calls []string
		if sw, ok := walker.(*SitterWalker); ok {
			if ctxAny := match.Context(); ctxAny != nil {
				if root, ok := ctxAny.(SitterRoot); ok {
					if c, err := sw.ExtractCalls(root.Node, root.Source, root.Lang, root.LangName); err == nil {
						calls = c
					}
					// Extract address-aware refs (env:, path:, url:) from the
					// match scope. These typed tokens bridge across languages
					// (e.g., Go os.Getenv calls within this function scope).
					if addrRefs, err := sw.ExtractAddressRefs(root.Node, root.Source, root.Lang, root.LangName); err == nil {
						calls = append(calls, addrRefs...)
					}
				}
			}
		}
		// Append file-level address refs (e.g., HCL variable declarations)
		// that weren't already found at the scope level. This avoids
		// duplicate refs when a Go function both calls os.Getenv and the
		// file-root also matches the same pattern.
		if len(fileAddressRefs) > 0 {
			scopeSeen := make(map[string]bool, len(calls))
			for _, c := range calls {
				scopeSeen[c] = true
			}
			for _, ref := range fileAddressRefs {
				if !scopeSeen[ref] {
					calls = append(calls, ref)
				}
			}
		}

		// Re-fetch current node (updated by recursion) — preserve Children + Properties
		var currentChildren []string
		var currentProps map[string][]byte
		if current, err := store.GetNode(id); err == nil {
			currentChildren = current.Children
			currentProps = current.Properties
		}

		// Pre-compute doc comments from backward scan (available to all file templates)
		docText, extStart, extEnd, hasScope := extractDocComments(match)

		node = &graph.Node{
			ID:         id,
			Mode:       os.ModeDir | 0o555, // Read-only dir
			ModTime:    modTime,            // Propagate source file time
			Children:   currentChildren,
			Context:    fileContext,
			Properties: currentProps,
		}

		// Set location property on directory node from source file's origin.
		// Read source bytes via DocScope so this is walker-agnostic (extStart/
		// extEnd already came from the walker-agnostic extractDocComments above).
		if hasScope && absSourceFile != "" {
			if ds, ok := match.(DocScope); ok {
				src := ds.ScopeSource()
				if node.Properties == nil {
					node.Properties = make(map[string][]byte)
				}
				relPath, err := filepath.Rel(e.RootPath, absSourceFile)
				if err == nil {
					startLine := byteOffsetToLine(src, extStart)
					endLine := byteOffsetToLine(src, extEnd)
					node.Properties["location"] = fmt.Appendf(nil, "%s:%d:%d", relPath, startLine, endLine)
				}
			}
		}
		store.AddNode(node)

		// Collect file children for batch write (single lock acquisition).
		var fileNodes []*graph.Node
		var sourceFileID string
		for _, fileSchema := range schema.Files {
			fileName, err := RenderTemplate(fileSchema.Name, match.Values())
			if err != nil {
				log.Printf("processNode: skip file name render %q: %v", fileSchema.Name, err) // coverage:ignore
				continue                                                                      // coverage:ignore
			}
			filePath := filepath.Join(currentPath, fileName)
			fileId := toNodeID(filePath)

			// Augment template values with doc comment text
			vals := match.Values()
			if docText != "" {
				vals["doc"] = docText // coverage:ignore
			} // coverage:ignore

			content, err := e.RenderContentTemplate(fileSchema.ContentTemplate, vals)
			if err != nil {
				log.Printf("processNode: skip file content render %q: %v", fileId, err) // coverage:ignore
				continue                                                                // coverage:ignore
			}

			// Skip empty optional files (e.g. "doc" when no doc comments exist)
			if content == "" && fileSchema.Name != "source" {
				continue // coverage:ignore
			}

			fileNode := &graph.Node{
				ID:      fileId,
				Mode:    0o444,
				ModTime: modTime,
				Data:    []byte(content),
			}

			// Extend source file content to include preceding doc comments.
			// Walker-agnostic via DocScope (extStart/extEnd came from the
			// walker-agnostic extractDocComments above).
			if hasScope && docText != "" && fileSchema.Name == "source" {
				if ds, ok := match.(DocScope); ok {
					src := ds.ScopeSource()
					if extEnd <= uint32(len(src)) {
						fileNode.Data = src[extStart:extEnd]
					}
				}
			}

			// Set write-back origin from backward scan
			if hasScope && absSourceFile != "" {
				fileNode.Origin = &graph.SourceOrigin{
					FilePath:  absSourceFile,
					StartByte: extStart,
					EndByte:   extEnd,
				}
			} else if op, ok := match.(OriginProvider); ok && absSourceFile != "" {
				// Fallback for non-sitter matches
				if start, end, ok := op.CaptureOrigin("scope"); ok { // coverage:ignore
					fileNode.Origin = &graph.SourceOrigin{ // coverage:ignore
						FilePath:  absSourceFile, // coverage:ignore
						StartByte: start,         // coverage:ignore
						EndByte:   end,           // coverage:ignore
					} // coverage:ignore
				} // coverage:ignore
			}

			fileNodes = append(fileNodes, fileNode)
			if fileSchema.Name == "source" {
				sourceFileID = fileId
			}
		}

		// Batch write: single lock acquisition for all file nodes + parent update.
		if len(fileNodes) > 0 {
			store.AddFileChildren(node, fileNodes)
		}

		// Refs AFTER batch (source file must exist in store first)
		if sourceFileID != "" {
			for _, token := range calls {
				if err := store.AddRef(token, sourceFileID); err != nil {
					return fmt.Errorf("add ref %s -> %s: %w", token, sourceFileID, err) // coverage:ignore
				} // coverage:ignore
			}
		}
	}
	return nil
}
