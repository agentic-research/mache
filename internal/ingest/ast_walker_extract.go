package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ExtractAddressRefs runs all registered address ref queries for the given
// language by querying the _ast table. Returns deduplicated, scheme-prefixed
// tokens (e.g., "env:DATABASE_URL"). Mirrors SitterWalker.ExtractAddressRefs
// but uses SQL instead of CGO tree-sitter.
func (w *ASTWalker) ExtractAddressRefs(sourcePath, langName string) ([]string, error) {
	raw, ok := addressRefRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	entries := raw.([]addressRefEntry)
	if len(entries) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
	root := ASTRoot{DB: w.db, SourceID: sourceID, ParentPrefix: ""}

	seen := make(map[string]bool)
	var tokens []string

	for _, entry := range entries {
		matches, err := w.Query(root, entry.Query)
		if err != nil {
			continue // selector may not be supported by ASTWalker
		}
		for _, m := range matches {
			vals := m.Values()
			refVal, ok := vals["ref"].(string)
			if !ok || refVal == "" {
				continue
			}
			value := unquoteCapture(refVal)
			if value == "" {
				continue
			}
			token := entry.Scheme + ":" + value
			if !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
	}

	return tokens, nil
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
func (w *ASTWalker) ExtractContext(sourcePath, langName string) ([]byte, error) {
	raw, ok := contextKindRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	kinds := raw.([]string)
	if len(kinds) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
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
