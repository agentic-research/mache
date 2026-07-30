// Package pkg is the install-verification fixture's only package.
//
// It is deliberately tiny and deliberately BORING: the install gate asserts on
// exact projection addresses (e.g. pkg/checksum.go/function_declaration_0),
// which are ordinal within the file. Reordering, inserting, or removing a
// top-level declaration here changes those addresses and will fail the gate —
// that is intended. Edit the expectations in mcp_test.go in the same commit.
package pkg

// ComputeChecksum sums the bytes of b. First top-level declaration in this
// file, hence function_declaration_0.
func ComputeChecksum(b []byte) int {
	total := 0
	for _, x := range b {
		total += int(x)
	}
	return total
}

// VerifyChecksum reports whether b sums to want. Second top-level declaration,
// hence function_declaration_1, and the only caller of ComputeChecksum.
func VerifyChecksum(b []byte, want int) bool {
	return ComputeChecksum(b) == want
}
