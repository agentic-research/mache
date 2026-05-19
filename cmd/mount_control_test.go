package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShortRoot_ZeroRoot returns the sentinel "<zero>" for an all-zero
// BLAKE3 root — the marker for "no snapshot published yet" in Control
// Block logs.
func TestShortRoot_ZeroRoot(t *testing.T) {
	var zero [32]byte
	require.Equal(t, "<zero>", shortRoot(zero))
}

// TestShortRoot_NonZero renders the first 4 bytes as 8 hex chars and
// ignores the remaining 28 bytes — that's the readable-prefix contract
// the helper provides for log lines.
func TestShortRoot_NonZero(t *testing.T) {
	var root [32]byte
	root[0] = 0xde
	root[1] = 0xad
	root[2] = 0xbe
	root[3] = 0xef
	// Bytes 4..31 must be ignored by the formatter.
	for i := 4; i < 32; i++ {
		root[i] = 0xff
	}
	require.Equal(t, "deadbeef", shortRoot(root))
}

// TestShortRoot_SingleNonZeroByte exercises the boundary case where only
// one byte is non-zero — IsZeroRoot must reject it (not the sentinel)
// and the formatter must still produce 8 hex chars.
func TestShortRoot_SingleNonZeroByte(t *testing.T) {
	var root [32]byte
	root[31] = 0x01
	require.Equal(t, "00000000", shortRoot(root))
}
