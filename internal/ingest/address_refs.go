package ingest

import (
	"strconv"
	"sync"
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

// unquoteCapture strips surrounding quotes from a tree-sitter string capture.
// Handles Go interpreted strings ("..."), HCL string_lit ("..."), and bare
// identifiers (returned unchanged). Returns empty string for empty quoted strings.
func unquoteCapture(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}
