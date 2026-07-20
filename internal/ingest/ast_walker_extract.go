package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ExtractAddressRefs runs all registered address ref queries for the given
// language by querying the _ast table (whole file). Returns deduplicated,
// scheme-prefixed tokens (e.g., "env:DATABASE_URL"). Mirrors
// SitterWalker.ExtractAddressRefs but uses SQL instead of CGO tree-sitter.
func (w *ASTWalker) ExtractAddressRefs(sourcePath, langName string) ([]string, error) {
	refs, err := w.fileAddrRefs(filepath.Base(sourcePath), langName)
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
// under scopeID (scopeID=="" = whole file). The trailing "/" guard matches the
// SQL LIKE semantics exactly — "func_1" does not match a call in "func_12".
func (w *ASTWalker) dedupAddrTokens(refs []scopedAddrRef, scopeID string) []string {
	prefix := scopeID + "/"
	seen := make(map[string]bool)
	var tokens []string
	for _, r := range refs {
		if scopeID != "" && r.nodeID != scopeID && !strings.HasPrefix(r.nodeID, prefix) {
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
// SitterWalker.ExtractContext but reads byte ranges from _ast and bytes
// from _source.
//
// Returns (nil, nil) when no context kinds are registered for the language.
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

	source, err := w.readSource(w.db, sourceID)
	if err != nil || source == nil {
		return nil, err
	}

	// Build placeholders for IN (?, ?, ...).
	placeholders := strings.Repeat("?,", len(kinds))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, sourceID)

	// Top-level nodes have no `/` in their id (or one segment after the
	// source root, depending on schema). We pull all matching kinds for
	// this source_id ordered by start_byte and slice the source bytes.
	query := fmt.Sprintf(`
		SELECT a.start_byte, a.end_byte
		FROM _ast a
		WHERE a.node_kind IN (%s) AND a.source_id = ?
		ORDER BY a.start_byte ASC`, placeholders)
	rows, err := w.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("extract context: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buf []byte
	seen := make(map[int]bool) // dedupe by start_byte
	for rows.Next() {
		var start, end int
		if err := rows.Scan(&start, &end); err != nil {
			continue
		}
		if seen[start] {
			continue
		}
		seen[start] = true
		if start < 0 || end > len(source) || start >= end {
			continue
		}
		buf = append(buf, source[start:end]...)
		buf = append(buf, '\n', '\n')
	}
	return buf, rows.Err()
}

// ExtractGoImports reads Go import aliases from the _imports table
// (produced by ley-line-open's `leyline parse`). Returns alias → path map
// for qualified call resolution (e.g., auth.Validate → github.com/foo/auth).
// Mirrors SitterWalker.ExtractGoImports but uses SQL instead of CGO tree-sitter.
func (w *ASTWalker) ExtractGoImports(sourceID string) (map[string]string, error) {
	// Check if _imports table exists (older .dbs produced before LLO didn't have it)
	var count int
	if err := w.db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_imports'",
	).Scan(&count); err != nil || count == 0 {
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
