package store

import (
	"sort"
	"strings"
)

// registry is written by both init() functions in this package.
var registry map[string]*Store

// Index returns the sorted keys of s.
func (s *Store) Index() []string {
	keys := make([]string, 0, len(s.items))
	for _, it := range s.items {
		keys = append(keys, it.Key)
	}
	sort.Strings(keys)
	return keys
}

// keyOf normalises a name into an index key.
func keyOf(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// init: second init() in package store → the projection must suffix one.
func init() {
	registry["default"] = New(&Config{Limit: 1})
}
