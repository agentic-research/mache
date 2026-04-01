package graph

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/agentic-research/mache/internal/refsvtab"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("node not found")

// ErrActNotSupported is returned by Graph implementations that do not support actions.
var ErrActNotSupported = errors.New("act not supported by this graph")

// ActionResult is returned when an action is performed on a graph node.
// Used by interactive graphs (browser DOM, iTerm2 sessions, macOS AX elements).
type ActionResult struct {
	NodeID  string `json:"node_id"`           // mache ID of the acted-upon node
	Action  string `json:"action"`            // "click", "type", "enter", "focus"
	Path    string `json:"path"`              // filesystem path
	Payload string `json:"payload,omitempty"` // optional (e.g., typed text)
}

// ContentRef is a recipe for lazily resolving file content from a backing store.
// Instead of storing the full byte content in RAM, we store enough info to re-fetch it on demand.
type ContentRef struct {
	DBPath     string // Path to the SQLite database
	RecordID   string // Row ID in the results table
	Template   string // Content template to re-render
	ContentLen int64  // Pre-computed rendered byte length
}

// SourceOrigin tracks the byte range of a construct in its source file.
// Used by write-back to splice edits into the original source.
type SourceOrigin struct {
	FilePath  string `json:"file"`
	StartByte uint32 `json:"start_byte"`
	EndByte   uint32 `json:"end_byte"`
}

// Node is the universal primitive.
// The Mode field explicitly declares whether this is a file or directory.
type Node struct {
	ID         string
	Mode       fs.FileMode       // fs.ModeDir for directories, 0 for regular files
	ModTime    time.Time         // Modification time
	Data       []byte            // Inline content (small files, nil for lazy nodes)
	Context    []byte            // Context content (imports/globals, for virtual 'context' file)
	DraftData  []byte            // Draft content (uncommitted/invalid edits)
	Ref        *ContentRef       // Lazy content reference (large files, nil for inline nodes)
	Properties map[string][]byte // Metadata / extended attributes
	Children   []string          // Child node IDs (directories only)
	Origin     *SourceOrigin     // Source byte range (nil for dirs, JSON, SQLite nodes)
}

// ContentSize returns the byte length of this node's content,
// regardless of whether it is inline or lazy.
func (n *Node) ContentSize() int64 {
	if n.DraftData != nil {
		return int64(len(n.DraftData))
	}
	if n.Data != nil {
		return int64(len(n.Data))
	}
	if n.Ref != nil {
		return n.Ref.ContentLen
	}
	return 0
}

// ContentResolverFunc resolves a ContentRef into byte content.
type ContentResolverFunc func(ref *ContentRef) ([]byte, error)

// QualifiedCall represents a function call with optional package qualifier.
type QualifiedCall struct {
	Token     string // Function/method name (e.g., "Validate")
	Qualifier string // Package qualifier (e.g., "auth"); empty for unqualified calls
}

// CallExtractor parses source code and returns qualified function call tokens.
// Used for on-demand "callees/" resolution.
// langName is the tree-sitter language identifier (e.g. "go", "python").
type CallExtractor func(content []byte, path, langName string) ([]QualifiedCall, error)

// Graph is the interface for the FUSE layer.
// This allows us to swap the backend later (Memory -> SQLite -> Mmap).
type Graph interface {
	GetNode(id string) (*Node, error)
	ListChildren(id string) ([]string, error)
	ReadContent(id string, buf []byte, offset int64) (int, error)
	GetCallers(token string) ([]*Node, error)
	GetCallees(id string) ([]*Node, error)
	// Invalidate evicts cached data for a node (size, content).
	// Called after write-back to force re-render on next access.
	Invalidate(id string)
	// Act performs an action on the node at the given path.
	// Interactive graphs (browser DOM, terminal sessions, macOS AX elements)
	// implement real actions. Passive graphs (code, data) return ErrActNotSupported.
	Act(id, action, payload string) (*ActionResult, error)
}

// -----------------------------------------------------------------------------
// Phase 1 Implementation: In-Memory Graph with Lazy Content Resolution
// -----------------------------------------------------------------------------

type MemoryStore struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	roots    []string            // Top-level nodes (e.g. "vulns")
	rootsSet map[string]struct{} // O(1) dedup for AddRoot
	resolver ContentResolverFunc
	cache    *contentCache
	refs     map[string][]string // token -> []nodeID (callers: who calls token)
	defs     map[string][]string // token -> []construct_dir_id (definitions: where token is defined)

	// Roaring bitmap index: file path → set of node internal IDs.
	// Enables O(k) DeleteFileNodes and ShiftOrigins instead of O(N) full scan.
	fileToNodes map[string]*roaring.Bitmap // FilePath → bitmap of internal node IDs
	nodeIntID   map[string]uint32          // Node.ID → internal bitmap uint32 ID
	intToNodeID []string                   // reverse: uint32 → Node.ID
	nextIntID   uint32                     // monotonic counter

	// Diagnostics: last write status per node path (for _diagnostics/ virtual dir).
	WriteStatus sync.Map // node path (string) → error message (string)

	// Temp-file SQLite sidecar for cross-reference queries.
	// Same schema as SQLiteGraph's .refs.db (node_refs + file_ids + mache_refs vtab).
	// Uses a temp file (not :memory:) because the vtab's xFilter needs a second
	// pool connection that can see the same tables — :memory: isolates per-connection.
	refsDB     *sql.DB
	refsDBPath string // temp file path, cleaned up on Close
	dbID       string // unique ID for vtab registry
	flushOnce  sync.Once
	flushErr   error

	extractor CallExtractor

	// Live graph: file mtime tracking and on-demand refresh.
	fileMtimes map[string]time.Time        // source file → mtime at index time
	refresher  func(filePath string) error // called when a source file is stale
	refreshMu  sync.Map                    // filePath → *sync.Mutex (per-file refresh serialization)
}

// normalizeID strips a leading slash from node IDs.
// GraphFS paths use "/Foo/source" but MemoryStore keys are "Foo/source".
func normalizeID(id string) string {
	if len(id) > 0 && id[0] == '/' {
		return id[1:]
	}
	return id
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes:       make(map[string]*Node),
		roots:       []string{},
		rootsSet:    make(map[string]struct{}),
		refs:        make(map[string][]string),
		defs:        make(map[string][]string),
		fileToNodes: make(map[string]*roaring.Bitmap),
		nodeIntID:   make(map[string]uint32),
		fileMtimes:  make(map[string]time.Time),
	}
}

// SetCallExtractor configures the parser for on-demand callee resolution.
func (s *MemoryStore) SetCallExtractor(fn CallExtractor) {
	s.extractor = fn
}

// SetResolver configures lazy content resolution for nodes with ContentRef.
// Cache size scales with node count: 25% of nodes, floor 1024, ceiling 16384.
func (s *MemoryStore) SetResolver(fn ContentResolverFunc) {
	s.resolver = fn
	size := len(s.nodes) / 4
	if size < 1024 {
		size = 1024
	}
	if size > 16384 {
		size = 16384
	}
	s.cache = newContentCache(size)
}

// RootIDs returns a copy of the top-level root node IDs.
func (s *MemoryStore) RootIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, len(s.roots))
	copy(ids, s.roots)
	return ids
}

// AddRoot registers a node as a top-level root and adds it to the store.
// Callers must explicitly declare roots — there is no heuristic.
func (s *MemoryStore) AddRoot(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	if _, dup := s.rootsSet[n.ID]; dup {
		return
	}
	s.rootsSet[n.ID] = struct{}{}
	s.roots = append(s.roots, n.ID)
}

// AddNode adds a non-root node to the store.
func (s *MemoryStore) AddNode(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	s.indexNode(n)
}

// AddFileChildren atomically adds file nodes and appends their IDs to the
// parent directory's Children slice. Single lock acquisition for the batch.
func (s *MemoryStore) AddFileChildren(parent *Node, files []*Node) {
	if len(files) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		s.nodes[f.ID] = f
		s.indexNode(f)
	}
	for _, f := range files {
		parent.Children = append(parent.Children, f.ID)
	}
	s.nodes[parent.ID] = parent
}

// ListChildNodes returns the full Node objects for all children of the given
// parent under a single RLock. Eliminates N individual GetNode calls during
// directory listing. Missing children are silently skipped.
func (s *MemoryStore) ListChildNodes(id string) ([]*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Root case
	var childIDs []string
	if id == "" || id == "/" {
		childIDs = s.roots
	} else {
		id = normalizeID(id)
		n, ok := s.nodes[id]
		if !ok {
			return nil, ErrNotFound
		}
		childIDs = n.Children
	}

	nodes := make([]*Node, 0, len(childIDs))
	for _, cid := range childIDs {
		if n, ok := s.nodes[cid]; ok {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// UpdateNodeContent surgically updates a node's content and origin in-place.
// Preserves Children, Context, Properties, and Ref. Clears DraftData on success.
func (s *MemoryStore) UpdateNodeContent(id string, data []byte, origin *SourceOrigin, modTime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = normalizeID(id)
	n, ok := s.nodes[id]
	if !ok {
		return ErrNotFound
	}
	n.Data = data
	n.DraftData = nil
	n.ModTime = modTime
	if origin != nil {
		n.Origin = origin
	}
	return nil
}

// UpdateNodeContext updates the Context field on a node (e.g., imports/package).
func (s *MemoryStore) UpdateNodeContext(id string, ctx []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = normalizeID(id)
	n, ok := s.nodes[id]
	if !ok {
		return ErrNotFound
	}
	n.Context = ctx
	return nil
}

// indexNode assigns an internal bitmap ID and registers the node in fileToNodes.
// Must be called with s.mu held.
func (s *MemoryStore) indexNode(n *Node) {
	if n.Origin == nil {
		return
	}
	// Assign internal ID if not already assigned
	intID, ok := s.nodeIntID[n.ID]
	if !ok {
		intID = s.nextIntID
		s.nextIntID++
		s.nodeIntID[n.ID] = intID
		// Grow reverse map
		for uint32(len(s.intToNodeID)) <= intID {
			s.intToNodeID = append(s.intToNodeID, "")
		}
		s.intToNodeID[intID] = n.ID
	}
	// Set bit in file→nodes bitmap
	bm, exists := s.fileToNodes[n.Origin.FilePath]
	if !exists {
		bm = roaring.New()
		s.fileToNodes[n.Origin.FilePath] = bm
	}
	bm.Add(intID)

	// Auto-record file mtime when indexing a node with Origin
	if n.Origin.FilePath != "" {
		if _, tracked := s.fileMtimes[n.Origin.FilePath]; !tracked {
			if info, err := os.Stat(n.Origin.FilePath); err == nil {
				s.fileMtimes[n.Origin.FilePath] = info.ModTime()
			}
		}
	}
}

// SetRefresher configures a callback invoked when a source file is stale.
// The callback should re-ingest the file and update the store.
func (s *MemoryStore) SetRefresher(fn func(filePath string) error) {
	s.refresher = fn
}

// RecordFileMtime explicitly records the mtime for a source file.
// Called after re-ingestion to update the tracked mtime.
func (s *MemoryStore) RecordFileMtime(filePath string, mtime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileMtimes[filePath] = mtime
}

// FileMtime returns the tracked mtime for a source file.
// Returns zero time if the file is not tracked.
func (s *MemoryStore) FileMtime(filePath string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileMtimes[filePath]
}

// IsFileStale returns true if the source file's current mtime differs
// from the tracked mtime (i.e., the file has been modified since indexing).
func (s *MemoryStore) IsFileStale(filePath string) bool {
	s.mu.RLock()
	tracked, ok := s.fileMtimes[filePath]
	s.mu.RUnlock()
	if !ok {
		return false // not tracked → not stale
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return true // can't stat (deleted?) → treat as stale
	}
	return !info.ModTime().Equal(tracked)
}

// AddRef records a reference from a file (nodeID) to a token.
func (s *MemoryStore) AddRef(token, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[token] = append(s.refs[token], nodeID)
	return nil
}

// AddDef records that a construct (dirID) defines the given token.
// Used by callees/ resolution: token → where it is defined.
// Uses copy-on-write: creates a new slice instead of appending to the existing one,
// so concurrent readers (GetCallees holds RLock) never see a partially-updated slice.
func (s *MemoryStore) AddDef(token, dirID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.defs[token]
	newSlice := make([]string, len(existing)+1)
	copy(newSlice, existing)
	newSlice[len(existing)] = dirID
	s.defs[token] = newSlice
	return nil
}

// RefsMap returns a snapshot of the token→nodeIDs reference map.
// Used by community detection to build the co-reference graph.
func (s *MemoryStore) RefsMap() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string][]string, len(s.refs))
	for k, v := range s.refs {
		cp[k] = append([]string(nil), v...)
	}
	return cp
}

// DefsMap returns a snapshot of the token→dirIDs definition map.
// Used by find_definition to locate where symbols are defined.
func (s *MemoryStore) DefsMap() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string][]string, len(s.defs))
	for k, v := range s.defs {
		cp[k] = append([]string(nil), v...)
	}
	return cp
}

// DeleteFileNodes removes all nodes that originated from the given source file.
// Uses the roaring bitmap index for O(k) lookup instead of O(N) full scan.
func (s *MemoryStore) DeleteFileNodes(filePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFileNodes(filePath)
}

// ReplaceFileNodes atomically replaces all nodes from a file with a new set.
// This prevents race conditions where files disappear during re-ingestion.
func (s *MemoryStore) ReplaceFileNodes(filePath string, newNodes []*Node) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleteFileNodes(filePath)

	for _, n := range newNodes {
		s.nodes[n.ID] = n
		s.indexNode(n)
	}
}

// deleteFileNodes performs deletion with lock already held.
func (s *MemoryStore) deleteFileNodes(filePath string) {
	// Canonicalize path to match Ingest behavior
	if realPath, err := filepath.EvalSymlinks(filePath); err == nil {
		filePath = realPath
	}

	// 1. Collect IDs to delete via bitmap index
	bm, hasBitmap := s.fileToNodes[filePath]
	var toDelete []string
	if hasBitmap {
		it := bm.Iterator()
		for it.HasNext() {
			intID := it.Next()
			if int(intID) < len(s.intToNodeID) {
				nodeID := s.intToNodeID[intID]
				if nodeID != "" {
					toDelete = append(toDelete, nodeID)
				}
			}
		}
	} else {
		// Fallback: full scan for nodes not yet indexed (e.g. added before indexing)
		for id, n := range s.nodes {
			if n.Origin != nil && n.Origin.FilePath == filePath {
				toDelete = append(toDelete, id)
			}
		}
	}

	// 2. Build deletion set for O(1) lookups
	deleteSet := make(map[string]struct{}, len(toDelete))
	for _, id := range toDelete {
		deleteSet[id] = struct{}{}
		delete(s.nodes, id)
		// Clean up bitmap index entries
		if intID, ok := s.nodeIntID[id]; ok {
			if hasBitmap {
				bm.Remove(intID)
			}
			delete(s.nodeIntID, id)
			if int(intID) < len(s.intToNodeID) {
				s.intToNodeID[intID] = ""
			}
		}
	}

	// Remove empty bitmap
	if hasBitmap && bm.IsEmpty() {
		delete(s.fileToNodes, filePath)
	}

	// 3. Clean up children pointers in remaining nodes
	for _, n := range s.nodes {
		if n.Mode.IsDir() && len(n.Children) > 0 {
			newChildren := n.Children[:0]
			changed := false
			for _, c := range n.Children {
				if _, del := deleteSet[c]; del {
					changed = true
				} else {
					newChildren = append(newChildren, c)
				}
			}
			if changed {
				n.Children = newChildren
			}
		}
	}

	// 4. Clean stale refs: remove deleted node IDs from token→[]nodeID map.
	// Without this, renamed/deleted functions persist as phantom callers.
	for token, nodeIDs := range s.refs {
		filtered := nodeIDs[:0]
		for _, nid := range nodeIDs {
			if _, del := deleteSet[nid]; !del {
				filtered = append(filtered, nid)
			}
		}
		if len(filtered) == 0 {
			delete(s.refs, token)
		} else if len(filtered) < len(nodeIDs) {
			s.refs[token] = filtered
		}
	}

	// 5. Clean stale defs: remove deleted dir IDs from token→[]dirID map.
	// Without this, renamed functions persist as phantom callees.
	for token, dirIDs := range s.defs {
		filtered := dirIDs[:0]
		for _, did := range dirIDs {
			if _, del := deleteSet[did]; !del {
				filtered = append(filtered, did)
			}
		}
		if len(filtered) == 0 {
			delete(s.defs, token)
		} else if len(filtered) < len(dirIDs) {
			s.defs[token] = filtered
		}
	}
}

// ShiftOrigins adjusts StartByte/EndByte for all nodes from filePath whose
// origin starts at or after afterByte. delta is the signed byte count change
// (positive = content grew, negative = content shrank).
// Called after splice, BEFORE re-ingest, to keep sibling offsets correct.
func (s *MemoryStore) ShiftOrigins(filePath string, afterByte uint32, delta int32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bm, ok := s.fileToNodes[filePath]
	if !ok {
		return
	}

	it := bm.Iterator()
	for it.HasNext() {
		intID := it.Next()
		if int(intID) >= len(s.intToNodeID) {
			continue
		}
		nodeID := s.intToNodeID[intID]
		if nodeID == "" {
			continue
		}
		n, exists := s.nodes[nodeID]
		if !exists || n.Origin == nil {
			continue
		}
		if n.Origin.FilePath != filePath {
			continue
		}
		// Only shift nodes that start at or after the splice point
		if n.Origin.StartByte >= afterByte {
			n.Origin.StartByte = uint32(int32(n.Origin.StartByte) + delta)
			n.Origin.EndByte = uint32(int32(n.Origin.EndByte) + delta)
		}
	}
}

// GetCallers implements Graph.
func (s *MemoryStore) GetCallers(token string) ([]*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, ok := s.refs[token]
	if !ok {
		return nil, nil
	}

	var nodes []*Node
	for _, id := range ids {
		if n, ok := s.nodes[id]; ok {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

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

// GetCallees implements Graph. It parses the node's source to find calls,
// then looks up those tokens in the defs index to find definitions.
func (s *MemoryStore) GetCallees(id string) ([]*Node, error) {
	// 1. Find the "source" file child
	s.mu.RLock()
	id = normalizeID(id)
	node, ok := s.nodes[id]
	s.mu.RUnlock()

	if !ok || !node.Mode.IsDir() {
		return nil, nil
	}

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

	// 3. Determine langName from construct directory Properties
	var langName string
	if node.Properties != nil {
		if v, ok := node.Properties["lang"]; ok {
			langName = string(v)
		}
	}

	// 4. Extract qualified calls
	if s.extractor == nil {
		return nil, nil
	}
	qcalls, err := s.extractor(buf, sourceID, langName)
	if err != nil {
		return nil, fmt.Errorf("extract calls: %w", err)
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

// Invalidate is a no-op for MemoryStore — nodes are updated in-place.
func (s *MemoryStore) Invalidate(id string) {}

// Act returns ErrActNotSupported — MemoryStore is a passive code graph.
func (s *MemoryStore) Act(id, action, payload string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}

// GetNode implements Graph.
func (s *MemoryStore) GetNode(id string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id = normalizeID(id)
	n, ok := s.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n, nil
}

// ListChildren implements Graph.
func (s *MemoryStore) ListChildren(id string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Root case
	if id == "" || id == "/" {
		return s.roots, nil
	}

	id = normalizeID(id)
	n, ok := s.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n.Children, nil
}

// ReadContent implements Graph. It handles both inline and lazy content.
// If the node has a SourceOrigin and the source file's mtime has changed,
// the refresher callback is invoked to re-ingest the file before reading.
func (s *MemoryStore) ReadContent(id string, buf []byte, offset int64) (int, error) {
	node, err := s.GetNode(id)
	if err != nil {
		return 0, err
	}

	// Live graph: check staleness for nodes with source origins
	if node.Origin != nil && node.Origin.FilePath != "" && s.refresher != nil {
		if s.IsFileStale(node.Origin.FilePath) {
			filePath := node.Origin.FilePath
			// Per-file mutex allows parallel refreshes of different files
			muI, _ := s.refreshMu.LoadOrStore(filePath, &sync.Mutex{})
			fileMu := muI.(*sync.Mutex)
			fileMu.Lock()
			// Double-check after acquiring lock (another goroutine may have refreshed)
			if s.IsFileStale(filePath) {
				if err := s.refresher(filePath); err != nil {
					log.Printf("live graph: refresh failed for %s: %v", filePath, err)
				}
			}
			fileMu.Unlock()
			// Re-fetch node after refresh (content may have changed)
			node, err = s.GetNode(id)
			if err != nil {
				return 0, err
			}
		}
	}

	var data []byte
	if node.DraftData != nil {
		data = node.DraftData
	} else if node.Data != nil {
		data = node.Data
	} else if node.Ref != nil {
		data, err = s.resolveContent(id, node.Ref)
		if err != nil {
			return 0, err
		}
	} else {
		return 0, nil
	}

	if offset >= int64(len(data)) {
		return 0, nil
	}
	end := offset + int64(len(buf))
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	n := copy(buf, data[offset:end])
	return n, nil
}

func (s *MemoryStore) resolveContent(id string, ref *ContentRef) ([]byte, error) {
	if s.cache != nil {
		if cached, ok := s.cache.get(id); ok {
			return cached, nil
		}
	}
	if s.resolver == nil {
		return nil, errors.New("no resolver configured for lazy content")
	}
	data, err := s.resolver(ref)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.put(id, data)
	}
	return data, nil
}

// contentCache is a simple FIFO-evicting bounded cache for resolved content.
// Uses RWMutex so concurrent readers (MCP tool calls) don't block each other.
//
// Note: the get→miss→resolve→put sequence in resolveContent is not atomic.
// Under high concurrency, multiple goroutines may miss the cache simultaneously
// and all invoke the resolver for the same key. This is benign — the first
// writer wins via put()'s dedup check, and subsequent resolver results are
// discarded. The resolver (SQLite query + template render) is idempotent.
// Use golang.org/x/sync/singleflight if redundant resolver calls become
// a measurable bottleneck.
type contentCache struct {
	mu      sync.RWMutex
	entries map[string][]byte
	keys    []string
	maxSize int
}

func newContentCache(maxSize int) *contentCache {
	return &contentCache{
		entries: make(map[string][]byte, maxSize),
		keys:    make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

func (c *contentCache) get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[key]
	return v, ok
}

func (c *contentCache) put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		c.entries[key] = value
		return
	}
	if len(c.entries) >= c.maxSize {
		// Copy to avoid backing-array leak from reslicing.
		evict := c.keys[0]
		copy(c.keys, c.keys[1:])
		c.keys = c.keys[:len(c.keys)-1]
		delete(c.entries, evict)
	}
	c.entries[key] = value
	c.keys = append(c.keys, key)
}

// ---------------------------------------------------------------------------
// MemoryStore SQL query support (in-memory SQLite sidecar)
// ---------------------------------------------------------------------------

// InitRefsDB opens an in-memory SQLite database with the same schema as
// SQLiteGraph's sidecar (node_refs + file_ids + mache_refs vtab).
// Must be called before FlushRefs. Safe to call multiple times (idempotent).
func (s *MemoryStore) InitRefsDB() error {
	if s.refsDB != nil {
		return nil
	}

	refsMod, err := refsvtab.Register()
	if err != nil {
		return err
	}

	// Use a temp file (not :memory:) because the vtab's xFilter runs inside
	// the SQLite engine on the outer connection and needs a SECOND pool
	// connection to query node_refs/file_ids. With :memory:, each connection
	// gets its own isolated database. A temp file + WAL mode lets both
	// connections see the same tables — same pattern as SQLiteGraph's .refs.db.
	tmpFile, err := os.CreateTemp("", "mache-refs-*.db")
	if err != nil {
		return fmt.Errorf("create temp refs db: %w", err)
	}
	refsPath := tmpFile.Name()
	_ = tmpFile.Close()

	db, err := sql.Open("sqlite", refsPath)
	if err != nil {
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("open refs db: %w", err)
	}
	// Allow 2 connections: one for normal queries, one for vtab Filter callbacks.
	// WAL mode ensures concurrent readers don't conflict.
	db.SetMaxOpenConns(2)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("set WAL mode on refs db: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (
			token TEXT PRIMARY KEY,
			bitmap BLOB
		);
		CREATE TABLE IF NOT EXISTS file_ids (
			id INTEGER PRIMARY KEY,
			path TEXT UNIQUE NOT NULL
		);
	`)
	if err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create refs tables: %w", err)
	}

	// Generate a unique ID for this DB connection to register with the vtab module.
	// This allows multiple MemoryStore instances (e.g. tests) to coexist without
	// race conditions on a single global refsDB pointer.
	dbID := fmt.Sprintf("mem_%d", time.Now().UnixNano())
	refsMod.RegisterDB(dbID, db)

	// Declare vtab with the unique ID as an argument.
	// The Create method in refs_module.go will look up the DB using this ID.
	query := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS mache_refs USING mache_refs(%s)", dbID)
	if _, err := db.Exec(query); err != nil {
		refsMod.UnregisterDB(dbID)
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create mache_refs vtab: %w", err)
	}

	s.refsDB = db
	s.refsDBPath = refsPath
	s.dbID = dbID
	return nil
}

// FlushRefs writes all accumulated refs (from AddRef) into the in-memory
// SQLite sidecar as roaring bitmaps. Guarded by sync.Once — safe to call
// multiple times; only the first call performs the flush.
func (s *MemoryStore) FlushRefs() error {
	s.flushOnce.Do(func() {
		s.flushErr = s.flushRefsInternal()
	})
	return s.flushErr
}

func (s *MemoryStore) flushRefsInternal() error {
	if s.refsDB == nil {
		return fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}

	s.mu.RLock()
	refs := s.refs
	s.mu.RUnlock()

	if len(refs) == 0 {
		return nil
	}

	// Build file ID map from all unique paths
	fileIDMap := make(map[string]uint32)
	var nextID uint32
	for _, paths := range refs {
		for _, p := range paths {
			if _, ok := fileIDMap[p]; !ok {
				fileIDMap[p] = nextID
				nextID++
			}
		}
	}

	// Build roaring bitmaps per token
	bitmaps := make(map[string]*roaring.Bitmap, len(refs))
	for token, paths := range refs {
		bm := roaring.New()
		for _, p := range paths {
			bm.Add(fileIDMap[p])
		}
		bitmaps[token] = bm
	}

	// Write both tables in a single transaction
	tx, err := s.refsDB.Begin()
	if err != nil {
		return fmt.Errorf("begin refs flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore (no-op if committed)

	fileStmt, err := tx.Prepare("INSERT OR IGNORE INTO file_ids (id, path) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare file_ids insert: %w", err)
	}
	defer func() { _ = fileStmt.Close() }() // safe to ignore

	for path, id := range fileIDMap {
		if _, err := fileStmt.Exec(id, path); err != nil {
			return fmt.Errorf("insert file_id %s: %w", path, err)
		}
	}

	refStmt, err := tx.Prepare("INSERT OR REPLACE INTO node_refs (token, bitmap) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare node_refs insert: %w", err)
	}
	defer func() { _ = refStmt.Close() }() // safe to ignore

	var buf bytes.Buffer
	for token, bm := range bitmaps {
		buf.Reset()
		if _, err := bm.WriteTo(&buf); err != nil {
			return fmt.Errorf("serialize bitmap for %s: %w", token, err)
		}
		if _, err := refStmt.Exec(token, buf.Bytes()); err != nil {
			return fmt.Errorf("insert ref %s: %w", token, err)
		}
	}

	return tx.Commit()
}

// QueryRefs executes a SQL query against the in-memory refs database,
// which includes the mache_refs virtual table.
func (s *MemoryStore) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if s.refsDB == nil {
		return nil, fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}
	return s.refsDB.Query(query, args...)
}

// Close closes the refs database and removes the temp file.
func (s *MemoryStore) Close() error {
	if s.refsDB != nil {
		// Unregister from vtab module to prevent leaks/races
		if mod, err := refsvtab.Register(); err == nil && mod != nil {
			mod.UnregisterDB(s.dbID)
		}

		err := s.refsDB.Close()
		if s.refsDBPath != "" {
			_ = os.Remove(s.refsDBPath) // best-effort cleanup
			_ = os.Remove(s.refsDBPath + "-wal")
			_ = os.Remove(s.refsDBPath + "-shm")
		}
		return err
	}
	return nil
}
