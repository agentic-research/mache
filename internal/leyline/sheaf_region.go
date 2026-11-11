package leyline

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/zeebo/blake3"
)

// The region-hash contract the ley-line daemon's sheaf cascade compares against
// (bead ley-line-open-1c5d28; LLO rs/ll-open/sheaf/src/region.rs, pinned by
// clients/go/leyline-schema/vectors/sheaf-region-v1.json).
//
// The daemon gates every restriction edge on an XOR pre-filter: the boundary
// (a, b) is unchanged while hash(a) XOR hash(b) still equals the edge's
// boundary_hash, and sheaf_defect counts every edge for which it does not.
// That is only meaningful when a region's hash is derived from its members'
// content and computed the same way at topology time and at invalidate time:
//
//	regionHash(R) = BLAKE3 derive_key("leyline partition fold v1", preimage)
//	preimage      = 0x03 (RowSet) || canon_version u32 LE (1)
//	                || len(scheme) u64 LE || "leyline/sheaf-region/v1"
//	                || len(parserId) u64 LE (32) || parserId
//	                || n u64 LE || for each member, sorted ascending by node_hash:
//	                     node_hash || 0 u64 LE || 0 u64 LE
//	boundaryHash(a, b) = regionHash(a) XOR regionHash(b)
//
// node_hash is ley-line's content-keyed merkle hash of the member, so an equal
// fold means equal member content; the members are a multiset (a duplicated
// node_hash counts twice); parserId binds the extraction epoch, so two parses
// under different parsers never compare equal. mache's former SHA-256 of
// member ids never satisfied this, so every edge it pushed was a defect and
// get_sheaf_status.defect carried no information (mache-a0d487).
const (
	regionScheme       = "leyline/sheaf-region/v1"
	regionFoldContext  = "leyline partition fold v1"
	regionCanonVersion = uint32(1)
	regionDomainRowSet = byte(0x03)
)

// ErrNoContentHashes is returned when a topology cannot be pushed because the
// graph exposes no node content hashes: only a ley-line projection records the
// parserId and node_hash a region hash folds over.
var ErrNoContentHashes = errors.New("the graph exposes no node content hashes (graph.ContentHasher); a sheaf region hash folds its members' node_hash under the parse's parserId, which only a ley-line projection records")

// ErrMemberWithoutNodeHash is returned when a region member has no node_hash.
// Such a region's content cannot be hashed, so no boundary through it can be
// computed, and the topology is refused rather than pushed with a boundary the
// daemon would count as a defect by construction.
var ErrMemberWithoutNodeHash = errors.New("a region member has no node_hash")

// RegionHash is the content-derived hash of one sheaf region: the keyed fold
// of its members' node_hash values under parserID, as declared above.
func RegionHash(parserID [32]byte, memberNodeHashes [][32]byte) [32]byte {
	var out [32]byte
	blake3.DeriveKey(regionFoldContext, regionPreimage(parserID, memberNodeHashes), out[:])
	return out
}

// regionPreimage is the bytes RegionHash folds, stated separately so the
// published vectors' preimage field is checked byte for byte.
func regionPreimage(parserID [32]byte, memberNodeHashes [][32]byte) []byte {
	sorted := slices.Clone(memberNodeHashes)
	slices.SortFunc(sorted, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })

	pre := make([]byte, 0, 1+4+8+len(regionScheme)+8+32+8+len(sorted)*(32+8+8))
	pre = append(pre, regionDomainRowSet)
	pre = binary.LittleEndian.AppendUint32(pre, regionCanonVersion)
	pre = binary.LittleEndian.AppendUint64(pre, uint64(len(regionScheme)))
	pre = append(pre, regionScheme...)
	pre = binary.LittleEndian.AppendUint64(pre, uint64(len(parserID)))
	pre = append(pre, parserID[:]...)
	pre = binary.LittleEndian.AppendUint64(pre, uint64(len(sorted)))
	for _, m := range sorted {
		pre = append(pre, m[:]...)
		pre = binary.LittleEndian.AppendUint64(pre, 0)
		pre = binary.LittleEndian.AppendUint64(pre, 0)
	}
	return pre
}

// BoundaryHash is the boundary_hash an edge between regions a and b carries
// at topology time: the bytewise XOR the daemon's pre-filter compares against.
func BoundaryHash(a, b [32]byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// HashHex is a hash as the sheaf wire carries it: 64 lowercase hex digits.
func HashHex(h [32]byte) string {
	return hex.EncodeToString(h[:])
}

// parseHash32 decodes a 32-byte hash from its 64 hex digits.
func parseHash32(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(raw) != len(out) {
		return out, fmt.Errorf("%d bytes, want %d", len(raw), len(out))
	}
	copy(out[:], raw)
	return out, nil
}
