package ingest

import (
	"strconv"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

// addressRefEntry binds a tree-sitter query to a token-scheme prefix.
// When the query matches, each captured @ref value is unquoted and prefixed
// with Scheme + ":" to produce a typed address ref token.
type addressRefEntry struct {
	Scheme string // e.g. "env", "path", "url"
	Query  string // tree-sitter S-expression with @ref captures
}

// addressRefRegistry stores per-language lists of address ref queries.
// Key: language name (string), Value: []addressRefEntry.
var addressRefRegistry sync.Map

// RegisterAddressRefQuery registers an address-aware ref extraction query
// for a specific language. The query must capture values as @ref.
// When matched, captured strings are unquoted (if quoted) and prefixed
// with scheme + ":" before being emitted as ref tokens.
//
// Multiple queries can be registered per language by calling this function
// multiple times; entries are appended.
func RegisterAddressRefQuery(langName, scheme, query string) {
	entry := addressRefEntry{Scheme: scheme, Query: query}
	for {
		existing, loaded := addressRefRegistry.Load(langName)
		if !loaded {
			if _, raced := addressRefRegistry.LoadOrStore(langName, []addressRefEntry{entry}); !raced {
				return
			}
			continue // another goroutine stored first; retry
		}
		entries := existing.([]addressRefEntry)
		entries = append(entries, entry)
		addressRefRegistry.Store(langName, entries)
		return
	}
}

// addressRefQueryCacheKey identifies a compiled address ref query by language
// name and scheme (since a language can have multiple address ref queries).
type addressRefQueryCacheKey struct {
	langName string
	scheme   string
}

// ExtractAddressRefs runs all registered address ref queries for the given
// language against the AST node. Returns deduplicated, scheme-prefixed tokens
// (e.g., "env:DATABASE_URL"). String captures are automatically unquoted.
func (w *SitterWalker) ExtractAddressRefs(root *sitter.Node, source []byte, lang *sitter.Language, langName string) ([]string, error) {
	raw, ok := addressRefRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	entries := raw.([]addressRefEntry)
	if len(entries) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var tokens []string

	for _, entry := range entries {
		q, err := w.getAddressRefQuery(lang, langName, entry)
		if err != nil {
			return nil, err
		}

		qc := sitter.NewQueryCursor()
		qc.Exec(q, root)

		for {
			m, ok := qc.NextMatch()
			if !ok {
				break
			}

			// Enforce #eq? / #not-eq? predicates.
			m = qc.FilterPredicates(m, source)
			if len(m.Captures) == 0 {
				continue
			}

			for _, c := range m.Captures {
				name := q.CaptureNameForId(c.Index)
				if name != "ref" {
					continue // skip anchor captures like @_pkg, @_func
				}

				start := c.Node.StartByte()
				end := c.Node.EndByte()
				if start >= uint32(len(source)) || end > uint32(len(source)) {
					continue
				}

				raw := string(source[start:end])
				value := unquoteCapture(raw)
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
		qc.Close()
	}

	return tokens, nil
}

// getAddressRefQuery returns a cached compiled query for an address ref entry.
func (w *SitterWalker) getAddressRefQuery(lang *sitter.Language, langName string, entry addressRefEntry) (*sitter.Query, error) {
	key := addressRefQueryCacheKey{langName: langName, scheme: entry.Scheme}
	if cached, ok := w.addressRefQueryCache.Load(key); ok {
		return cached.(*sitter.Query), nil
	}

	q, err := sitter.NewQuery([]byte(entry.Query), lang)
	if err != nil {
		return nil, err
	}
	actual, loaded := w.addressRefQueryCache.LoadOrStore(key, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// unquoteCapture strips surrounding quotes from a tree-sitter string capture.
// Handles Go interpreted strings ("..."), HCL string_lit ("..."), and bare
// identifiers (returned unchanged). Returns empty string for empty quoted strings.
func unquoteCapture(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}
