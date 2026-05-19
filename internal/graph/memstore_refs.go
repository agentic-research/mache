package graph

// AddRef records a reference from a file (nodeID) to a token.
func (s *MemoryStore) AddRef(token, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[token] = append(s.refs[token], nodeID)
	// Invalidate the RefsMap snapshot — see RefsMap doc.
	s.refsSnap.Store(nil)
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
	// Invalidate the DefsMap snapshot — the next reader rebuilds.
	// Done under the write lock so the invalidation cannot race
	// with a concurrent DefsMap-snapshot store (which holds RLock,
	// blocking this Lock until the store completes).
	s.defsSnap.Store(nil)
	return nil
}

// RefsMap returns a snapshot of the token→nodeIDs reference map.
// Used by community detection to build the co-reference graph.
//
// Memoized: the first call after an invalidation builds the deep-
// copy snapshot; subsequent calls return the same instance until
// the next AddRef / DeleteFileNodes. Callers MUST treat the
// returned map as read-only — mutating it corrupts the cache for
// the next caller. Every existing caller (community detection,
// composite graph wiring) is read-only.
func (s *MemoryStore) RefsMap() map[string][]string {
	if cached := s.refsSnap.Load(); cached != nil {
		return *cached
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Re-check under RLock — another goroutine may have built the
	// snapshot between our Load above and the lock acquisition.
	if cached := s.refsSnap.Load(); cached != nil {
		return *cached
	}
	cp := make(map[string][]string, len(s.refs))
	for k, v := range s.refs {
		cp[k] = append([]string(nil), v...)
	}
	s.refsSnap.Store(&cp)
	return cp
}

// DefsMap returns a snapshot of the token→dirIDs definition map.
// Used by find_definition's case-insensitive and fuzzy paths and by
// search role=definition — anywhere callers need to iterate. The
// anchored-exact path in find_definition should use LookupDef
// instead to avoid the O(N) snapshot copy.
//
// Memoized — see RefsMap for the cache contract. Same read-only
// constraint applies. Callers that need a mutable copy should
// either deep-copy the result themselves or use LookupDef for
// per-token lookups.
func (s *MemoryStore) DefsMap() map[string][]string {
	if cached := s.defsSnap.Load(); cached != nil {
		return *cached
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cached := s.defsSnap.Load(); cached != nil {
		return *cached
	}
	cp := make(map[string][]string, len(s.defs))
	for k, v := range s.defs {
		cp[k] = append([]string(nil), v...)
	}
	s.defsSnap.Store(&cp)
	return cp
}

// LookupDef returns the dir IDs that define `token` without
// snapshotting the entire defs map. Returns nil for unknown tokens.
// O(1) under the read lock; the returned slice is a copy so callers
// can mutate it without races. Pair with DefsMap when the caller
// needs to iterate (case-insensitive matching, fuzzy substring,
// etc.); use this when one specific token's IDs are wanted.
func (s *MemoryStore) LookupDef(token string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids, ok := s.defs[token]
	if !ok {
		return nil
	}
	return append([]string(nil), ids...)
}

// SearchDefs returns up to `limit` token→nodeIDs entries whose token
// matches the SQL LIKE pattern. Mirrors SQLiteGraph.SearchDefs for
// dispatch uniformity — every backend that has a defs map should
// also be searchable, so the handler's defsSearcher check resolves
// the same regardless of backend (bead from PR #373 post-fix audit:
// lazyGraph wrapping a MemoryStore previously returned nil from
// SearchDefs because MemoryStore didn't implement it, and the
// handler's defsMapProvider fallback was unreachable).
func (s *MemoryStore) SearchDefs(pattern string, limit int) map[string][]string {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string)
	for token, ids := range s.defs {
		if !likeMatch(pattern, token) {
			continue
		}
		out[token] = append([]string(nil), ids...)
		if len(out) >= limit {
			return out
		}
	}
	return out
}
