package graph

import "path/filepath"

// Node deletion for MemoryStore.
//
// Two entry points, because a node can be identified two ways and only one of
// them is a file. DeleteFileNodes finds a file's nodes through the file index,
// which carries only nodes with an Origin — so it reaches a construct's source
// leaf and never the construct DIRECTORY that holds it. DeleteNodes takes the
// IDs directly, which is how the ingest engine hands back the constructs a
// re-ingested file claimed (mache-399c25).

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

	s.deleteNodeSet(toDelete)

	// Remove empty bitmap
	if bm, ok := s.fileToNodes[filePath]; ok && bm.IsEmpty() {
		delete(s.fileToNodes, filePath)
	}
}

// DeleteNodes removes the named nodes and everything that pointed at them,
// whatever file they came from.
//
// DeleteFileNodes cannot do this job. It finds nodes through the file→nodes
// bitmap, which indexNode only populates for nodes carrying an Origin — and a
// CONSTRUCT DIRECTORY has no Origin, only its source leaf does. So a re-ingest
// replaced `fns/alpha/source` and left `fns/alpha` behind as an empty husk,
// forever, one per edit (mache-399c25). The engine knows exactly which
// construct IDs a file claimed; this is how it hands them back.
func (s *MemoryStore) DeleteNodes(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteNodeSet(ids)
}

// deleteNodeSet removes ids and scrubs every index that referenced them.
// Caller holds s.mu.
func (s *MemoryStore) deleteNodeSet(toDelete []string) {
	if len(toDelete) == 0 {
		return
	}
	deleteSet := s.forgetNodes(toDelete)
	s.unlinkFromParents(deleteSet)
	// Without these, renamed or deleted functions persist as phantom callers
	// and callees.
	pruneTokenIndex(s.refs, deleteSet)
	pruneTokenIndex(s.defs, deleteSet)

	// Invalidate both snapshots — a deletion can mutate either or both maps.
	// Done at the end so any partial-write window doesn't surface to a
	// concurrent reader; the AddDef/AddRef invariant (write lock held during
	// invalidate) holds here too.
	s.defsSnap.Store(nil)
	s.refsSnap.Store(nil)
}

// forgetNodes removes each node and its index entries, returning the set of
// IDs removed. Caller holds s.mu.
func (s *MemoryStore) forgetNodes(toDelete []string) map[string]struct{} {
	deleteSet := make(map[string]struct{}, len(toDelete))
	for _, id := range toDelete {
		deleteSet[id] = struct{}{}
		delete(s.nodes, id)
		intID, ok := s.nodeIntID[id]
		if !ok {
			continue
		}
		// Drop the bit from whichever file bitmap holds it. A node with no
		// Origin is in no bitmap at all, which is exactly why DeleteNodes
		// exists.
		for _, bm := range s.fileToNodes {
			bm.Remove(intID)
		}
		delete(s.nodeIntID, id)
		if int(intID) < len(s.intToNodeID) {
			s.intToNodeID[intID] = ""
		}
	}
	return deleteSet
}

// unlinkFromParents drops the deleted IDs from their parents' Children.
//
// Visits only the parents of deleted nodes, derived from the IDs themselves.
// This was an O(N) scan over the whole store until mache-07f9ca. Caller holds
// s.mu.
func (s *MemoryStore) unlinkFromParents(deleteSet map[string]struct{}) {
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
		kept := n.Children[:0]
		changed := false
		for _, c := range n.Children {
			if _, del := deleteSet[c]; del {
				changed = true
			} else {
				kept = append(kept, c)
			}
		}
		if changed {
			n.Children = kept
		}
	}
}

// pruneTokenIndex removes the deleted IDs from a token to node-ID index,
// dropping any token left pointing at nothing. Shared by the refs and defs
// maps, which are the same shape and need the same treatment.
func pruneTokenIndex(index map[string][]string, deleteSet map[string]struct{}) {
	for token, ids := range index {
		kept := ids[:0]
		for _, id := range ids {
			if _, del := deleteSet[id]; !del {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(index, token)
		} else if len(kept) < len(ids) {
			index[token] = kept
		}
	}
}
