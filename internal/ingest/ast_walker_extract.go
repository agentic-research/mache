package ingest

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ExtractAddressRefs runs all registered address ref queries for sourceID and
// language by querying the _ast table (whole file). sourceID is leyline's
// root-relative _source.id (for example "sub/nested.go"), not an arbitrary OS
// path. Returns deduplicated, scheme-prefixed tokens such as
// "env:DATABASE_URL".
func (w *ASTWalker) ExtractAddressRefs(sourceID, langName string) ([]string, error) {
	refs, err := w.fileAddrRefs(sourceID, langName)
	if err != nil {
		return nil, err
	}
	return w.dedupAddrTokens(refs, ""), nil
}

// ExtractAddressRefsScoped is the per-construct equivalent: address refs whose
// matched node lives under scopeID's id-path. Mirrors SitterWalker's per-scope
// ExtractAddressRefs(scopeNode, ...). sourceID is the _source key.
//
// It filters the whole-file address-ref set (computed once and cached per file)
// by node-id prefix in Go rather than re-running the generic Query per
// construct. The old per-construct path re-scanned every call_expression in
// every function — 84% of a whole-repo projection's CPU (mache-4f3840). The
// prefix filter (scopeID+"/") is byte-identical to the SQL `n.id LIKE scopeID
// || '/%'` the per-construct Query used, so the projected refs are unchanged.
func (w *ASTWalker) ExtractAddressRefsScoped(sourceID, scopeID, langName string) ([]string, error) {
	refs, err := w.fileAddrRefs(sourceID, langName)
	if err != nil {
		return nil, err
	}
	return w.dedupAddrTokens(refs, scopeID), nil
}

// dedupAddrTokens returns the deduplicated tokens from refs whose node lives
// STRICTLY UNDER scopeID (scopeID=="" = whole file). The trailing "/" prefix is
// byte-identical to the old per-construct SQL `n.id LIKE scopeID||'/%'` (which
// never self-matches the scope node) and to the ExtractCallsScoped sibling
// (mache-702f9b — dropped an earlier exact-self-match carve-out that diverged
// from both). "func_1" does not match a call in "func_12".
func (w *ASTWalker) dedupAddrTokens(refs []scopedAddrRef, scopeID string) []string {
	prefix := scopeID + "/"
	seen := make(map[string]bool)
	var tokens []string
	for _, r := range refs {
		if scopeID != "" && !strings.HasPrefix(r.nodeID, prefix) {
			continue
		}
		if !seen[r.token] {
			seen[r.token] = true
			tokens = append(tokens, r.token)
		}
	}
	return tokens
}

// fileAddrRefs computes every address ref in the whole file ONCE (running each
// registered selector against the full file, not per construct) and caches the
// result keyed by (sourceID, lang). Each match carries the id of the node it
// was captured on so per-construct callers can attribute by prefix.
func (w *ASTWalker) fileAddrRefs(sourceID, langName string) ([]scopedAddrRef, error) {
	key := sourceID + "\x00" + langName
	if v, ok := w.addrRefCache.Load(key); ok {
		return v.([]scopedAddrRef), nil
	}

	var refs []scopedAddrRef
	if raw, ok := addressRefRegistry.Load(langName); ok {
		if entries := raw.([]addressRefEntry); len(entries) > 0 {
			root := ASTRoot{DB: w.db, SourceID: sourceID, ParentPrefix: ""} // whole file
			for _, entry := range entries {
				matches, err := w.Query(root, entry.Query)
				if err != nil {
					// The registered address-ref selectors are all ASTWalker-
					// supported, so an error is a real/transient DB failure, not
					// an unsupported selector. Surface it and DON'T cache the
					// partial set (mache-015f5c) — a silently-empty cache would
					// permanently drop this file's address refs on a serve.
					return nil, fmt.Errorf("file address refs %s/%s (%s): %w",
						sourceID, langName, entry.Scheme, err)
				}
				for _, m := range matches {
					refVal, ok := m.Values()["ref"].(string)
					if !ok || refVal == "" {
						continue
					}
					value := unquoteCapture(refVal)
					if value == "" {
						continue
					}
					// Query sets ctx.ParentPrefix to the matched @scope node id
					// (the outer node when no @scope capture is present) — the
					// id we attribute the ref to.
					nodeID := ""
					if ar, ok := m.Context().(ASTRoot); ok {
						nodeID = ar.ParentPrefix
					}
					refs = append(refs, scopedAddrRef{nodeID: nodeID, token: entry.Scheme + ":" + value})
				}
			}
		}
	}

	w.addrRefCache.Store(key, refs)
	return refs, nil
}

// contextKindRegistry stores per-language top-level node kinds whose source
// text should be concatenated into the context blob (Go imports, consts,
// vars, type declarations, etc.).
var contextKindRegistry sync.Map // langName → []string (node_kinds)

// RegisterASTContextKinds registers the top-level node kinds whose source
// bytes constitute the context blob for the given language.
func RegisterASTContextKinds(langName string, kinds []string) {
	contextKindRegistry.Store(langName, kinds)
}

// ExtractContext returns the concatenated source text of the top-level
// context nodes for the given file (e.g. Go's import/const/var/type
// declarations). Used by schema context fields. Mirrors
// SitterWalker.ExtractContext but slices the file's section: the node kinds
// from its index, the bytes from its _source row.
//
// Returns (nil, nil) when no context kinds are registered for the language,
// and (nil, err) when the file's source cannot be read.
//
// sourceID is the _source/_ast key (the path relative to the ingest root, as
// ley-line produces it) — NOT a filesystem path. Callers that hold a path use
// Engine.sourceIDFor to derive it (mache-30edfa).
func (w *ASTWalker) ExtractContext(sourceID, langName string) ([]byte, error) {
	raw, ok := contextKindRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	kinds := raw.([]string)
	if len(kinds) == 0 {
		return nil, nil
	}
	idx, err := w.fileSection(sourceID)
	if err != nil {
		return nil, fmt.Errorf("extract context: %w", err)
	}
	if idx.srcErr != nil {
		return nil, idx.srcErr
	}
	return idx.contextText(kinds), nil
}

// ExtractGoImports reads Go import aliases from the _imports table
// (produced by ley-line-open's `leyline parse`). Returns alias → path map
// for qualified call resolution (e.g., auth.Validate → github.com/foo/auth).
// Mirrors SitterWalker.ExtractGoImports but uses SQL instead of CGO tree-sitter.
//
// Older .dbs produced before LLO grew _imports have no such table; that is
// probed once per walker (not once per file), and resolves to no imports.
func (w *ASTWalker) ExtractGoImports(sourceID string) (map[string]string, error) {
	has, err := w.hasImportsTable()
	if err != nil {
		return nil, fmt.Errorf("probe _imports: %w", err)
	}
	if !has {
		return nil, nil
	}

	rows, err := w.db.Query(
		"SELECT alias, path FROM _imports WHERE source_id = ?", sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query _imports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	imports := make(map[string]string)
	for rows.Next() {
		var alias, path string
		if err := rows.Scan(&alias, &path); err != nil {
			continue
		}
		imports[alias] = path
	}
	return imports, rows.Err()
}

// hasImportsTable reports whether the db has an _imports table, asking
// sqlite_master once and remembering the answer; a failed probe is not
// remembered, so a transient failure does not latch the walker into "no
// imports" for its lifetime.
func (w *ASTWalker) hasImportsTable() (bool, error) {
	w.importsMu.Lock()
	defer w.importsMu.Unlock()
	if w.importsProbed {
		return w.hasImports, nil
	}
	var count int
	if err := w.db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_imports'",
	).Scan(&count); err != nil {
		return false, err
	}
	w.importsProbed, w.hasImports = true, count > 0
	return w.hasImports, nil
}

// packageName is the Go package name: the first package_identifier's text
// (nodes.record, else the _source byte range). "" when the file has none.
func (idx *fileIndex) packageName() string {
	ids := idx.byKind["package_identifier"]
	if len(ids) == 0 {
		return ""
	}
	n := idx.all[ids[0]]
	if n.record != "" {
		return n.record
	}
	if n.startByte >= 0 && n.startByte < n.endByte && n.endByte <= len(idx.source) {
		return string(idx.source[n.startByte:n.endByte])
	}
	return ""
}

// docExtendStart walks backward from scopeID over contiguous preceding comment
// siblings (same parent, <= 2 byte gap), returning the doc-extended start
// byte. A scope the index does not hold (the "$" grouping match has none)
// extends nothing. Note that leyline writes no comment rows (mache-a83451),
// so against today's substrate this always returns scopeStart.
func (idx *fileIndex) docExtendStart(scopeID string, scopeStart uint32) uint32 {
	i, ok := idx.byID[scopeID]
	if !ok {
		return scopeStart
	}
	cs := idx.comments[idx.all[i].parentID]
	start := int(scopeStart)
	// Candidates end at or before start; end_byte is monotone along cs (see
	// the field comment), so the closest one is the last of that prefix.
	k := sort.Search(len(cs), func(j int) bool { return idx.all[cs[j]].endByte > start })
	for ; k > 0; k-- {
		c := idx.all[cs[k-1]]
		if start-c.endByte > 2 {
			break
		}
		start = c.startByte
	}
	return uint32(start)
}

// contextText concatenates the source text of every node whose kind is in
// kinds, in start_byte order, one blank line after each — the context blob
// (Go's import/const/var/type declarations). Nodes sharing a start byte
// contribute once.
func (idx *fileIndex) contextText(kinds []string) []byte {
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	var buf []byte
	seen := make(map[int]bool)
	for _, n := range idx.all {
		if !want[n.astKind] || seen[n.startByte] {
			continue
		}
		seen[n.startByte] = true
		if n.startByte < 0 || n.endByte > len(idx.source) || n.startByte >= n.endByte {
			continue
		}
		buf = append(buf, idx.source[n.startByte:n.endByte]...)
		buf = append(buf, '\n', '\n')
	}
	return buf
}
