package lattice

import "github.com/RoaringBitmap/roaring"

// MaxConcepts is the safety cap on concept enumeration.
// If the lattice has more concepts than this, enumeration stops early.
const MaxConcepts = 10000

// Concept is a maximal rectangle in the incidence table:
// a pair (Extent, Intent) where Extent' = Intent and Intent' = Extent.
type Concept struct {
	Extent *roaring.Bitmap // object indices
	Intent *roaring.Bitmap // attribute indices
}

// NextClosure enumerates all formal concepts using Ganter's algorithm.
// Concepts are produced in lectic order of their intents.
// Output-polynomial: O(|concepts| × |M| × |G|).
func NextClosure(ctx *FormalContext) []Concept {
	n := len(ctx.Attributes)
	if n == 0 {
		return nil
	}

	// Start with closure of empty set
	empty := roaring.New()
	firstIntent := ctx.Closure(empty)
	firstExtent := ctx.AttrDeriv(firstIntent)

	concepts := []Concept{{Extent: firstExtent, Intent: firstIntent}}

	current := firstIntent.Clone()
	for len(concepts) < MaxConcepts {
		next := nextClosedSet(ctx, current, n)
		if next == nil {
			break
		}
		extent := ctx.AttrDeriv(next)
		concepts = append(concepts, Concept{Extent: extent, Intent: next})
		current = next
	}

	return concepts
}

// nextClosedSet finds the next closed set after current in lectic order.
// Returns nil if current is the last (i.e., the full attribute set M).
func nextClosedSet(ctx *FormalContext, current *roaring.Bitmap, n int) *roaring.Bitmap {
	for i := n - 1; i >= 0; i-- {
		ui := uint32(i)
		if current.Contains(ui) {
			continue
		}
		// B = (current ∩ {0,...,i-1}) ∪ {i}
		b := roaring.New()
		iter := current.Iterator()
		for iter.HasNext() {
			j := iter.Next()
			if j < ui {
				b.Add(j)
			}
		}
		b.Add(ui)

		// C = closure(B)
		c := ctx.Closure(b)

		// Canonicity test: C ∩ {0,...,i-1} must equal current ∩ {0,...,i-1}
		// i.e., closing B didn't add any attribute with index < i
		valid := true
		for j := uint32(0); j < ui; j++ {
			if c.Contains(j) != current.Contains(j) {
				valid = false
				break
			}
		}
		if valid {
			return c
		}
	}
	return nil
}
