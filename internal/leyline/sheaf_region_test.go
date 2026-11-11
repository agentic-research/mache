package leyline

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leylineSchemaModule is the Go module that publishes the sheaf-region vectors
// beside the daemon wire types mache already decodes with.
const leylineSchemaModule = "github.com/agentic-research/ley-line-open/clients/go/leyline-schema"

// regionVectors is clients/go/leyline-schema/vectors/sheaf-region-v1.json.
type regionVectors struct {
	Doc              string `json:"_doc"`
	DeriveKeyContext string `json:"derive_key_context"`
	Scheme           string `json:"scheme"`
	CanonVersion     uint32 `json:"canon_version"`
	Cases            []struct {
		Name             string   `json:"name"`
		ParserID         string   `json:"parser_id"`
		MemberNodeHashes []string `json:"member_node_hashes"`
		Preimage         string   `json:"preimage"`
		RegionHash       string   `json:"region_hash"`
	} `json:"cases"`
	Boundary struct {
		A        string `json:"a_region_hash"`
		B        string `json:"b_region_hash"`
		Boundary string `json:"boundary_hash"`
	} `json:"boundary"`
}

// loadRegionVectors reads the vectors from the pinned module's directory, not
// from a copy: a leyline bump that moves the contract moves this test.
func loadRegionVectors(t *testing.T) regionVectors {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", leylineSchemaModule).Output()
	require.NoError(t, err, "locate the pinned %s module", leylineSchemaModule)
	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir)
	raw, err := os.ReadFile(filepath.Join(dir, "vectors", "sheaf-region-v1.json"))
	require.NoError(t, err)
	var v regionVectors
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotEmpty(t, v.Cases, "the vector file names no cases")
	return v
}

func mustHash32(t *testing.T, s string) [32]byte {
	t.Helper()
	h, err := parseHash32(s)
	require.NoError(t, err, s)
	return h
}

// TestRegionHash_MatchesThePublishedVectors: every case of the vendored
// sheaf-region-v1 vectors, preimage and hash, byte for byte, and the boundary.
// RegionHash is what PushTopology sends and what the daemon XORs, so this is
// the one test that says mache and leyline agree on what a region hash is.
func TestRegionHash_MatchesThePublishedVectors(t *testing.T) {
	v := loadRegionVectors(t)
	assert.Equal(t, regionFoldContext, v.DeriveKeyContext)
	assert.Equal(t, regionScheme, v.Scheme)
	assert.Equal(t, regionCanonVersion, v.CanonVersion)

	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			parser := mustHash32(t, c.ParserID)
			members := make([][32]byte, len(c.MemberNodeHashes))
			for i, m := range c.MemberNodeHashes {
				members[i] = mustHash32(t, m)
			}
			assert.Equal(t, c.Preimage, hex.EncodeToString(regionPreimage(parser, members)), "preimage")
			assert.Equal(t, c.RegionHash, HashHex(RegionHash(parser, members)), "region_hash")
		})
	}

	a, b := mustHash32(t, v.Boundary.A), mustHash32(t, v.Boundary.B)
	assert.Equal(t, v.Boundary.Boundary, HashHex(BoundaryHash(a, b)), "boundary_hash")
	assert.Equal(t, v.Cases[0].RegionHash, v.Boundary.A, "the boundary's a side is the first case's region")
}

// TestRegionHash_IsAFunctionOfTheMemberMultisetAndParser pins the properties
// the daemon's pre-filter relies on, independent of the vector file.
func TestRegionHash_IsAFunctionOfTheMemberMultisetAndParser(t *testing.T) {
	h := func(b byte) [32]byte {
		var out [32]byte
		for i := range out {
			out[i] = b
		}
		return out
	}
	parser := h(0x5a)
	base := RegionHash(parser, [][32]byte{h(1), h(2)})
	assert.Equal(t, base, RegionHash(parser, [][32]byte{h(2), h(1)}), "order is erased")
	assert.NotEqual(t, base, RegionHash(parser, [][32]byte{h(1), h(3)}), "content moves it")
	assert.NotEqual(t, base, RegionHash(parser, [][32]byte{h(1)}), "a removed member moves it")
	assert.NotEqual(t, RegionHash(parser, [][32]byte{h(1)}), RegionHash(parser, [][32]byte{h(1), h(1)}), "a duplicate counts")
	assert.NotEqual(t, base, RegionHash(h(0x5b), [][32]byte{h(1), h(2)}), "parserId is bound")
	assert.Equal(t, [32]byte{}, BoundaryHash(base, base), "a boundary of equal sides is zero")
}

func TestParseHash32_RefusesWhatIsNotAHash(t *testing.T) {
	for _, s := range []string{"", "zz", strings.Repeat("ab", 31), strings.Repeat("ab", 33)} {
		_, err := parseHash32(s)
		assert.Error(t, err, s)
	}
	h, err := parseHash32(strings.Repeat("ab", 32))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("ab", 32), HashHex(h))
}
