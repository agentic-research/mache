package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
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

// idToPath is the inverse of toNodeID for rebuilding a filesystem path from a
// claimed node ID.
func idToPath(id string) string {
	return filepath.FromSlash(id)
}

// claimConstructID reserves a node ID for a content-bearing construct and
// returns the ID to actually use, disambiguating when the name is taken.
//
// Guarantee: this never returns an ID already handed out during this Ingest, so
// a construct can never be overwritten by a later one. That is the invariant —
// the specific suffix is not.
//
// Disambiguation prefers the existing ".from_<file>" shape, which is meaningful
// when two files contribute the same name (multiple Go init()s in one package).
// It falls back to a numeric suffix only when that is ALSO taken, which is the
// same-file case the old code could not express at all: dedupSuffix is a
// function of the source file alone, so N>2 constructs sharing a name within one
// file all rendered the identical suffixed ID and collided again.
//
// PROVISIONAL. A numeric suffix is not a good identity — it is positional, so
// inserting a construct above renumbers the ones below. It exists to stop data
// loss now, not to settle addressing. The real fix is for the name to be unique
// in the first place: receiver- and module-qualified construct names
// (mache-c777ef), sourced from ley-line's node_defs rather than re-derived per
// language, moving toward the (semantic address, node_hash) identity in
// mache-e64f36. Collisions are logged at WARN precisely so that work stays
// visible rather than being papered over here.
//
// Only content-bearing nodes are claimed. A schema node with a static name and
// no files — a package or category directory matched once per record — is
// SUPPOSED to collapse so its children merge under one parent; suffixing those
// would shatter every directory in the projection.
func (e *Engine) claimConstructID(id, parentPath, name, sourceFile string) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.claimedIDs == nil {
		e.claimedIDs = make(map[string]int)
	}
	if _, taken := e.claimedIDs[id]; !taken {
		e.claimedIDs[id] = 1
		return id
	}

	candidate := id
	if sourceFile != "" {
		suffixed := toNodeID(filepath.Join(parentPath, name+dedupSuffix(sourceFile)))
		if _, taken := e.claimedIDs[suffixed]; !taken {
			e.claimedIDs[suffixed] = 1
			log.Printf("[WARN] duplicate construct name %q under %q — emitting %q; "+
				"the schema's name template is not unique for this language (mache-c777ef)",
				name, parentPath, suffixed)
			return suffixed
		}
		candidate = suffixed
	}

	// Same name, same file: neither the bare name nor the file-suffixed form is
	// free. Walk a counter until one is.
	for n := 2; ; n++ {
		numbered := fmt.Sprintf("%s.%d", candidate, n)
		if _, taken := e.claimedIDs[numbered]; !taken {
			e.claimedIDs[numbered] = 1
			log.Printf("[WARN] duplicate construct name %q under %q in %s — emitting %q; "+
				"same-file collision, so the name carries no distinguishing information "+
				"(mache-c777ef)", name, parentPath, sourceFile, numbered)
			return numbered
		}
	}
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

// listChildrenTolerant returns a node's children, treating "node not found" as
// "no children" rather than an error.
//
// The call sites replaced GetNode()+read-the-field, which ignored a lookup
// failure entirely (`if existing, err := store.GetNode(id); err == nil`). A
// node that has not been created yet legitimately has no children, and the
// backends disagree about whether asking is an error: MemoryStore returns
// ErrNotFound, SQLiteWriter returns an empty set. Normalising the accessor
// means normalising that too, otherwise the fix trades a silent wrong answer
// for a spurious hard failure (mache-e3d9bb).
func listChildrenTolerant(store IngestionTarget, id string) ([]string, error) {
	kids, err := store.ListChildren(id)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return kids, nil
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
		// Skip self-match if requested (e.g. for recursive schemas to avoid
		// infinite loops). On the AST path a match's scope is identified by
		// its node id (ASTRoot.ParentPrefix), so parent==child when the
		// resolved scope node id is identical and non-empty.
		if schema.SkipSelfMatch {
			if parentRoot, ok := ctx.(ASTRoot); ok { // coverage:ignore
				if childCtx, ok := match.Context().(ASTRoot); ok { // coverage:ignore
					if parentRoot.ParentPrefix != "" && // coverage:ignore
						parentRoot.ParentPrefix == childCtx.ParentPrefix { // coverage:ignore
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

		// Dedup: two constructs whose schema name template renders the same
		// string want the same node ID, and whoever writes last wins — the
		// earlier ones vanish with no error and no diagnostic.
		//
		// This used to gate on `store.GetNode(id).Children`, which is
		// unreachable on the build path: SQLiteWriter.GetNode never populates
		// Children (it selects kind/mtime/record/context/props and nothing
		// else), so the condition was always false and the branch was dead.
		// MemoryStore returns the live node, so it DID fire there — one engine,
		// two backends, two different graphs (mache-e3d9bb). Measured cost of
		// the dead branch: 733 unaddressable Rust constructs (16.9%), and 9 of
		// mache's own cmd/ init() functions absent from its own graph
		// (mache-c725e9).
		//
		// Claimed IDs are tracked in the engine, not read back from the store,
		// following the e.childSeen precedent below — collision detection must
		// not depend on which backend is being written to.
		if len(schema.Files) > 0 {
			if claimed := e.claimConstructID(id, parentPath, name, sourceFile); claimed != id {
				id = claimed
				currentPath = idToPath(claimed)
			}
		}

		// Create/Update Node — preserve existing children when merging
		// multiple files into the same node (e.g. multiple .go files in one package).
		//
		// Via ListChildren, NOT GetNode().Children. Children have one accessor
		// because the stores disagree on the other: MemoryStore returns the live
		// node so Children is populated, every SQLite-backed store rebuilds from
		// one row and leaves it nil. Reading the field made this merge inert on
		// the build path while working in memory (mache-e3d9bb).
		existingChildren, err := listChildrenTolerant(store, id)
		if err != nil {
			return fmt.Errorf("list children of %s: %w", id, err)
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
				graph.SetPropString(node, "lang", l)

				// Go package name, for qualified def resolution.
				if pkg := fm.PackageName(); pkg != "" {
					graph.SetPropString(node, "pkg", pkg)
				}
			}
		}

		// Store structured imports (avoids regex re-parsing at query time).
		// Independent of walker type — persist whenever fileImports is non-nil.
		if fileImports != nil {
			if importJSON, err := json.Marshal(fileImports); err == nil {
				graph.SetPropRaw(node, "imports", importJSON)
			}
		}
		store.AddNode(node)

		// Register definition: construct name → directory ID
		if len(schema.Files) > 0 {
			if err := store.AddDef(name, id); err != nil {
				return fmt.Errorf("add def %s -> %s: %w", name, id, err) // coverage:ignore
			} // coverage:ignore
			// Register qualified definition (package.name → directory ID)
			{
				if pkg := graph.PropString(node, "pkg"); pkg != "" {
					qualKey := pkg + "." + name
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
			// Guard the childSeen + parent.Children read-modify-write against
			// concurrent ReIngestFile (watcher, two files under this parent) —
			// same rationale as engine_ingest.go (mache-706757).
			e.mu.Lock()
			parent, err := store.GetNode(parentId)
			if err == nil {
				if e.childSeen[parentId] == nil {
					// Seed from ListChildren rather than parent.Children: the
					// field is nil on every SQLite-backed store, so seeding from
					// it silently started this set empty on the build path.
					existing, lerr := listChildrenTolerant(store, parentId)
					if lerr != nil {
						e.mu.Unlock()
						return fmt.Errorf("list children of %s: %w", parentId, lerr)
					}
					e.childSeen[parentId] = make(map[string]bool, len(existing))
					for _, c := range existing {
						e.childSeen[parentId][c] = true
					}
				}
				if !e.childSeen[parentId][id] {
					e.childSeen[parentId][id] = true
					parent.Children = append(parent.Children, id)
					store.AddNode(parent)
				}
			}
			e.mu.Unlock()
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

		// Extract per-scope calls + address-aware refs (env:, path:, url:) for
		// the refs index. Read via the CallExtractor interface so the engine is
		// walker-agnostic — both sitterMatch (tree-sitter scope-node query) and
		// astMatch (scope-prefixed SQL) implement it, and parentAwareMatch
		// forwards. Replaces the prior walker.(*SitterWalker) type-switch.
		var calls []string
		if ce, ok := match.(CallExtractor); ok {
			calls = ce.ScopeCalls()
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

		// Re-fetch current node (updated by recursion) — preserve Children +
		// Properties. Children come from ListChildren, Properties from GetNode:
		// they have different contracts. Properties round-trips through the
		// props column on every store; Children does NOT round-trip through
		// GetNode on any SQLite-backed one, so reading it here silently
		// discarded the children the recursion had just added on the build path
		// (mache-e3d9bb).
		currentChildren, err := listChildrenTolerant(store, id)
		if err != nil {
			return fmt.Errorf("list children of %s: %w", id, err)
		}
		var currentProps map[string]json.RawMessage
		if current, gerr := store.GetNode(id); gerr == nil {
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
				relPath, err := filepath.Rel(e.RootPath, absSourceFile)
				if err == nil {
					startLine := byteOffsetToLine(src, extStart)
					endLine := byteOffsetToLine(src, extEnd)
					graph.SetPropString(node, "location",
						fmt.Sprintf("%s:%d:%d", relPath, startLine, endLine))
				}
			}
		}

		// Persist the AST scope mapping (real _ast source_id + scope node id)
		// onto the construct so serve-time find_callees can recover a scoped+
		// qualified callee query directly from the graph node, instead of
		// re-deriving it from the graph node id — which is NOT an _ast key and
		// produced zero rows every time (bead mache-fd9982). Only astMatch
		// (pure-Go, _ast-backed) implements ASTScope; sitterMatch (CGO
		// tree-sitter) needs no such mapping since its extractor re-parses
		// already-scoped content bytes directly.
		//
		// Only persist for a real LEAF construct scope — not for:
		//   - "$" grouping matches (functions/, types/, imports/ container
		//     dirs): these have no real @scope node, so hasScope (computed
		//     above via extractDocComments/DocRange) is false for them.
		//   - the package-root match itself, whose captured @scope IS the
		//     entire source_file node — ley-line assigns that root AST node
		//     the SAME id as the file's _source.id (verified empirically:
		//     `_ast` row "pkg.go|pkg.go|source_file|0|85"), so scopeID==srcID
		//     is the whole-file signal.
		// Without this guard, grouping dirs and the package root inherit
		// ast_scope_id = the whole file, and find_callees on them queries
		// the scoped extractor over the ENTIRE file — a whole-file call
		// union that's nondeterministic (last-file-wins) for multi-file
		// packages (bead mache-6fbaf1, F3).
		if as, ok := match.(ASTScope); ok {
			if srcID, scopeID := as.ASTSourceID(), as.ASTScopeID(); srcID != "" && scopeID != "" && scopeID != srcID && hasScope {
				graph.SetPropString(node, "ast_source_id", srcID)
				graph.SetPropString(node, "ast_scope_id", scopeID)
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
