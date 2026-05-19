package graph

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/RoaringBitmap/roaring"
)

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
//
// Copy-on-write: publishes a NEW *Node into the map rather than mutating the
// caller's parent in place. Readers that obtained the previous *Node pointer
// from GetNode see an immutable snapshot of the old Children slice; subsequent
// GetNode callers see the updated node. This avoids a data race where a
// reader holds a *Node pointer (lock released) while a writer appends to the
// same Children slice.
//
// Side effect: the caller's `parent` pointer is stale after this call —
// further mutations via that pointer are not reflected in the store.
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

	// Use the canonical parent already in the map (if any). The caller's
	// pointer may be a stale copy from an earlier GetNode.
	canonical, ok := s.nodes[parent.ID]
	if !ok {
		canonical = parent
	}
	newChildren := make([]string, 0, len(canonical.Children)+len(files))
	newChildren = append(newChildren, canonical.Children...)
	for _, f := range files {
		newChildren = append(newChildren, f.ID)
	}
	updated := *canonical
	updated.Children = newChildren
	s.nodes[parent.ID] = &updated
}

// UpdateNodeContent surgically updates a node's content and origin in-place.
// Preserves Children, Context, Properties, and Ref. Clears DraftData on success.
func (s *MemoryStore) UpdateNodeContent(id string, data []byte, origin *SourceOrigin, modTime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = NormalizeID(id)
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

	id = NormalizeID(id)
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

// DeleteFileNodes removes all nodes that originated from the given source file.
// Uses the roaring bitmap index for O(k) lookup instead of O(N) full scan.
func (s *MemoryStore) DeleteFileNodes(filePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFileNodes(filePath)
}

// NodesForPath returns the IDs of every node whose origin file is filePath,
// in deterministic order. The path is canonicalised via filepath.EvalSymlinks
// to match the bookkeeping Ingest does. Returns an empty slice when no nodes
// are indexed under that path.
//
// O(k) via the file→nodes bitmap; falls back to a full O(N) scan only when a
// path was added before the bitmap was populated (the same fallback path
// DeleteFileNodes uses, kept for parity).
//
// Used by the file-watcher → SheafInvalidator wiring (cmd/serve.go) so the
// sheaf cascade has a node ID to look up the region for. Exposed via the
// NodesForPathProvider interface so non-MemoryStore backends can plug in
// without leaking *MemoryStore type assertions into serve.go.
func (s *MemoryStore) NodesForPath(filePath string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	canonical := filePath
	if realPath, err := filepath.EvalSymlinks(filePath); err == nil {
		canonical = realPath
	}

	var ids []string
	if bm, ok := s.fileToNodes[canonical]; ok {
		it := bm.Iterator()
		for it.HasNext() {
			intID := it.Next()
			if int(intID) < len(s.intToNodeID) {
				nodeID := s.intToNodeID[intID]
				if nodeID != "" {
					ids = append(ids, nodeID)
				}
			}
		}
		sort.Strings(ids)
		return ids
	}

	// Fallback: linear scan for paths not in the bitmap yet.
	for id, n := range s.nodes {
		if n.Origin != nil && n.Origin.FilePath == canonical {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
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

	// 3. Clean up Children pointers in just the parents of deleted nodes.
	// Was an O(N) scan over the whole store (bead mache-07f9ca); now
	// O(K) over the unique parent paths derived from the deleted IDs.
	parentsTouched := make(map[string]struct{}, len(deleteSet))
	for id := range deleteSet {
		if pid := parentOfNodeID(id); pid != "" {
			parentsTouched[pid] = struct{}{}
		}
	}
	for pid := range parentsTouched {
		n, ok := s.nodes[pid]
		if !ok || !n.Mode.IsDir() || len(n.Children) == 0 {
			continue
		}
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

	// Invalidate both snapshots — DeleteFileNodes can mutate either
	// or both maps. Done at end so any partial-write window doesn't
	// surface to a concurrent reader; the AddDef/AddRef invariant
	// (write lock held during invalidate) holds here too.
	s.defsSnap.Store(nil)
	s.refsSnap.Store(nil)
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
			newStart := int32(n.Origin.StartByte) + delta
			newEnd := int32(n.Origin.EndByte) + delta
			if newStart < 0 {
				newStart = 0
			}
			if newEnd < 0 {
				newEnd = 0
			}
			n.Origin.StartByte = uint32(newStart)
			n.Origin.EndByte = uint32(newEnd)
		}
	}
}
