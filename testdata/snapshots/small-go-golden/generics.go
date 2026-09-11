package main

// Generic receivers. Before mache-51571b the go preset's methods/ selectors
// stopped at pointer_type/type_identifier, so every method on a generic type
// was projected NOWHERE — not methods/, not functions/, no routing warning.

// Stack has one pointer-receiver and one value-receiver method, the two shapes
// the non-generic selectors already cover, wrapped in a generic_type.
type Stack[T any] struct{ items []T }

func (s *Stack[T]) Push(v T) { s.items = append(s.items, v) }

func (s Stack[T]) Len() int { return len(s.items) }

// Pair carries TWO type parameters, so the receiver name must come from the
// generic_type's type_identifier child and not from the type arguments.
type Pair[K comparable, V any] struct {
	k K
	v V
}

func (p *Pair[K, V]) Swap() { p.k, p.v = p.k, p.v }
