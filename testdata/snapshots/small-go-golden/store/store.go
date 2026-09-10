package store

import "errors"

// maxItems bounds a Store regardless of Config.Limit.
const maxItems = 8

// ErrFull is returned by Add when the store is at capacity.
var ErrFull = errors.New("store: full")

// Config parameterises New.
type Config struct {
	Limit int
}

// Adder is implemented by *Store.
type Adder interface {
	Add(name string) error
}

// Item is one stored entry.
type Item struct {
	Key  string
	Name string
}

// Store is a bounded, ordered set of items.
type Store struct {
	limit int
	items []Item
}

// New returns a Store bounded by cfg.Limit (capped at maxItems).
func New(cfg *Config) *Store {
	limit := cfg.Limit
	if limit > maxItems {
		limit = maxItems
	}
	return &Store{limit: limit}
}

// Add appends name; pointer receiver.
func (s *Store) Add(name string) error {
	if len(s.items) >= s.limit {
		return ErrFull
	}
	s.items = append(s.items, Item{Key: keyOf(name), Name: name})
	return nil
}

// Len reports the item count; value receiver.
func (s Store) Len() int {
	return len(s.items)
}

func init() {
	registry = map[string]*Store{}
}
