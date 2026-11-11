package ingest

import (
	"bytes"
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

// docExtendStart returns where scopeID's doc comment starts: the run of
// comment siblings directly above it, with no blank line inside the run or
// before the construct, or scopeStart when there is none. A scope the index
// does not hold (the "$" grouping match has none), or a file whose source the
// projection does not hold, extends nothing.
//
// It is LLO's recipe for a definition's doc comment (TABLE_CONTRACT, "Comments
// at projection-v13"), which comment occurrences make possible from leyline
// v0.29.0 (mache-1e3d11; before it _ast had no comment rows, mache-a83451).
// A comment is any comment kind (lloschema.IsCommentKind), so Rust's and
// Java's line_comment and block_comment are docs as Go's comment is. Rust
// attributes sit between an item and its doc (`/// doc` `#[derive]` `struct`),
// so the run reaches over attribute_item siblings to a comment above them; an
// attribute with no comment above it extends nothing. An inner doc comment
// (`//!`, `/*!`) documents what encloses it, not what follows, so it ends the
// run.
func (idx *fileIndex) docExtendStart(scopeID string, scopeStart uint32) uint32 {
	i, ok := idx.byID[scopeID]
	if !ok || idx.source == nil {
		return scopeStart
	}
	parent := idx.all[i].parentID
	comments, siblings := idx.comments[parent], idx.children[parent]
	doc, cur := int(scopeStart), int(scopeStart)
	for {
		// The nearest comment and the nearest code sibling ending at or
		// before cur. Siblings never overlap, so end_byte is monotone along
		// both lists and each is a bisection.
		c := sort.Search(len(comments), func(j int) bool { return comments[j].end > cur })
		k := sort.Search(len(siblings), func(j int) bool { return idx.all[siblings[j]].endByte > cur })
		switch {
		case c > 0 && (k == 0 || comments[c-1].end > idx.all[siblings[k-1]].endByte):
			prev := comments[c-1]
			if !idx.adjoins(prev.end, cur) || idx.innerDoc(prev) {
				return uint32(doc)
			}
			doc, cur = prev.start, prev.start
		case k > 0 && idx.all[siblings[k-1]].astKind == "attribute_item":
			prev := idx.all[siblings[k-1]]
			if !idx.adjoins(prev.endByte, cur) {
				return uint32(doc)
			}
			cur = prev.startByte
		default:
			return uint32(doc)
		}
	}
}

// innerDoc reports whether the comment at c is an inner doc comment.
func (idx *fileIndex) innerDoc(c span) bool {
	text := idx.source[c.start:c.end]
	return bytes.HasPrefix(text, []byte("//!")) || bytes.HasPrefix(text, []byte("/*!"))
}

// adjoins reports whether the text ending at end and the text starting at next
// are on consecutive or the same lines with only whitespace between: no blank
// line. A Rust line_comment's span ends after its newline, so that newline
// counts as the one between them.
func (idx *fileIndex) adjoins(end, next int) bool {
	if end < 0 || end > next || next > len(idx.source) {
		return false
	}
	breaks := 0
	if end > 0 && idx.source[end-1] == '\n' {
		breaks++
	}
	for _, b := range idx.source[end:next] {
		switch b {
		case '\n':
			breaks++
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return breaks <= 1
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
