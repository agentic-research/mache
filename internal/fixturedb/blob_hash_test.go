package fixturedb

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSourceBlobHash_IsLeylinesBlake3 pins a fixture's blob hash to the one
// leyline writes for the same bytes: `leyline parse` (v0.24.0) of a file
// holding "package a\n" stores _tree.blob_hash and _source.content_hash
// 65648300…, and the second vector came from the same check. A fixture with
// any other hash (it was SHA-256) carries keys no real arena has
// (mache-10a306).
func TestSourceBlobHash_IsLeylinesBlake3(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"package a\n", "65648300f68d69cd24b54ec62ea078c6ac953b35e53ebd6d2c2e4da697a24ad9"},
		{"package b\n\nfunc B() int { return 2 }\n", "f24231c7a01f68d3b781b48aee63010728631aa1e8acd68582e677b0ae27be30"},
	} {
		for _, p := range []Producer{Leyline, LeylineV6} {
			b := New(t, p)
			b.Source("a.go", "go", tc.content)
			_, f := b.Build()
			var got []byte
			require.NoError(t, f.DB().QueryRow(
				`SELECT b.blob_hash FROM _source s JOIN source_blobs b ON b.blob_hash = s.content_hash WHERE s.id = 'a.go'`).Scan(&got))
			assert.Equal(t, tc.want, hex.EncodeToString(got), "%s, content %q", p, tc.content)
		}
	}
}
