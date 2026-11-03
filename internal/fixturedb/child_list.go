package fixturedb

import "slices"

// childRow is one `node_child` row as the fixture states it: the child's hash
// and the field it sits under.
type childRow struct {
	hash  []byte
	field string
}

// sameChildList reports whether two child lists are the same list: same
// hashes in the same order under the same fields.
func sameChildList(a, b []childRow) bool {
	return slices.EqualFunc(a, b, func(x, y childRow) bool {
		return slices.Equal(x.hash, y.hash) && x.field == y.field
	})
}
