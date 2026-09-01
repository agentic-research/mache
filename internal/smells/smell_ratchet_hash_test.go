package smells

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hf builds a finding carrying a content address.
func hf(ruleID, sourceID, nodeHash string, line int) smellFinding {
	return smellFinding{RuleID: ruleID, SourceID: sourceID, NodeHash: nodeHash, Line: line}
}

// TestRatchet_MovedDebtStaysGrandfathered is failure direction 1 from
// mache-dd45a3: move an offender to a new file and the path-keyed ratchet
// called every relocated finding NEW debt, because the new path had no entry.
// The debt did not grow — it moved — and the gate blocked the refactor.
//
// Content addressing makes it invariant by construction: same code, same
// hash, wherever it lives. That the hash is path-independent is not assumed
// here — it was measured, an identical function at a/thing.go and
// b/deep/moved.go hashing identically.
func TestRatchet_MovedDebtStaysGrandfathered(t *testing.T) {
	base := computeBaseline([]smellFinding{
		hf("long_function", "cmd/big.go", "aaa", 10),
		hf("long_function", "cmd/big.go", "bbb", 40),
		hf("dead_code", "cmd/big.go", "ccc", 70),
	})

	// The identical three findings, now living in two new files.
	moved := []smellFinding{
		hf("long_function", "internal/split/one.go", "aaa", 3),
		hf("long_function", "internal/split/two.go", "bbb", 8),
		hf("dead_code", "internal/split/two.go", "ccc", 20),
	}

	assert.Empty(t, newDebt(moved, base),
		"relocating unchanged code must produce zero new debt — the gate must not block a pure move")
}

// TestRatchet_NewDebtInAMovedFileIsStillReported is the other half: move
// invariance must not become blanket amnesty. A genuinely new finding in the
// same commit as a move still has to fail the gate, or the fix would have
// traded a false positive for a false negative.
func TestRatchet_NewDebtInAMovedFileIsStillReported(t *testing.T) {
	base := computeBaseline([]smellFinding{
		hf("long_function", "cmd/big.go", "aaa", 10),
	})

	current := []smellFinding{
		hf("long_function", "internal/split/one.go", "aaa", 3),  // moved: grandfathered
		hf("long_function", "internal/split/one.go", "zzz", 50), // genuinely new
	}

	debt := newDebt(current, base)
	require.Len(t, debt, 1, "the new finding must be reported even though its file also received moved debt")
	assert.Equal(t, "zzz", debt[0].NodeHash)
}

// TestRatchet_VacatedEntryCannotGrandfatherFreshDebt is failure direction 2 —
// the laundering. Under path keying the vacated cmd/big.go entry persisted at
// its old count forever and nothing reclaimed it, so N fresh findings could be
// added back to that path and pass. Keyed on content, an entry whose hash is
// no longer present in the tree matches nothing.
func TestRatchet_VacatedEntryCannotGrandfatherFreshDebt(t *testing.T) {
	base := computeBaseline([]smellFinding{
		hf("long_function", "cmd/big.go", "aaa", 10),
		hf("long_function", "cmd/big.go", "bbb", 40),
	})

	// The old code moved away; brand-new, different code appears at the old path.
	current := []smellFinding{
		hf("long_function", "cmd/big.go", "new1", 5),
		hf("long_function", "cmd/big.go", "new2", 25),
	}

	debt := newDebt(current, base)
	assert.Len(t, debt, 2,
		"fresh findings must not inherit an allowance left behind by code that moved away — "+
			"that is the laundering path keying allowed")
}

// TestRatchet_V1BaselineStillGates pins the migration. A v1 file is
// path-keyed and has no hashes; loading one must keep working exactly as
// before. A migration that made every existing entry miss would fail the gate
// on the first refactor after upgrading — the very failure being fixed.
func TestRatchet_V1BaselineStillGates(t *testing.T) {
	v1 := smellBaseline{Version: 1, Counts: []baselineEntry{
		{RuleID: "long_function", SourceID: "cmd/big.go", Count: 2},
	}}

	// Findings DO carry hashes now (leyline backend), but a v1 baseline must
	// still be read path-keyed or nothing would match.
	current := []smellFinding{
		hf("long_function", "cmd/big.go", "aaa", 10),
		hf("long_function", "cmd/big.go", "bbb", 40),
	}
	assert.Empty(t, newDebt(current, v1), "a v1 baseline must keep gating path-keyed")

	current = append(current, hf("long_function", "cmd/big.go", "ccc", 90))
	assert.Len(t, newDebt(current, v1), 1, "and must still catch growth")
}

// TestRatchet_StandaloneBackendFallsBackToPath documents and pins the
// degraded mode. A producer with no `_ast` writes no node_hash, so findings
// arrive with an empty one; those stay path-keyed and behave exactly as they
// did before v2. Explicitly degraded, not silently broken.
func TestRatchet_StandaloneBackendFallsBackToPath(t *testing.T) {
	base := computeBaseline([]smellFinding{
		{RuleID: "long_function", SourceID: "a.go", Line: 10},
		{RuleID: "long_function", SourceID: "a.go", Line: 40},
	})
	require.Equal(t, baselineVersion, base.Version)
	for _, e := range base.Counts {
		assert.Empty(t, e.NodeHash, "no producer hash means no entry hash")
	}

	assert.Empty(t, newDebt([]smellFinding{
		{RuleID: "long_function", SourceID: "a.go", Line: 11},
		{RuleID: "long_function", SourceID: "a.go", Line: 41},
	}, base), "path keying still grandfathers in-place debt")

	// And without a content address, a move is NOT invariant — the honest
	// consequence of a backend that cannot say what the content is.
	assert.Len(t, newDebt([]smellFinding{
		{RuleID: "long_function", SourceID: "b.go", Line: 11},
		{RuleID: "long_function", SourceID: "b.go", Line: 41},
	}, base), 2, "a standalone backend cannot recognise a move; that limit is the documented trade")
}

// TestRatchet_HashAndPathKeysCannotCollide pins the prefixing. A content
// address and a file path are different KINDS of claim; letting one satisfy
// the other would let a moved file spend an allowance twice.
func TestRatchet_HashAndPathKeysCannotCollide(t *testing.T) {
	same := "identical"
	assert.NotEqual(t,
		ratchetKey("r", same, "other", baselineVersion),
		ratchetKey("r", "", same, baselineVersion),
		"a hash key and a path key with the same string must remain distinct")
}

// TestRatchet_BaselineRoundTripsHashes guards the wire format: a hash that
// does not survive write+read would silently revert the whole gate to path
// keying, and every test above would still pass in memory.
func TestRatchet_BaselineRoundTripsHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	orig := computeBaseline([]smellFinding{
		hf("long_function", "cmd/big.go", "aaa", 10),
		{RuleID: "dead_code", SourceID: "c.go", Line: 3}, // no hash: path-keyed
	})
	require.NoError(t, writeBaseline(path, orig))

	got, err := loadBaseline(path)
	require.NoError(t, err)
	assert.Equal(t, baselineVersion, got.Version)
	assert.Equal(t, 1, got.lookup(hashKey("long_function", "aaa")))
	assert.Equal(t, 1, got.lookup(pathKey("dead_code", "c.go")))
}

// TestRatchet_FileLevelFindingsStayPathKeyed pins a trap found by measuring
// rather than reasoning. File-level rules (god_file, long_file) emit no
// node_id, and the file's own `_ast` row does have a hash — so keying them on
// it looks natural. It is wrong: a file's merkle hash covers its whole
// content, so ANY edit changes it (verified: appending one function moved a
// file hash from 6e1abfb0… to 15986e62…).
//
// Keyed that way, a god_file entry would stop matching the moment anyone
// touched the file, and the next PR editing any of the thirteen current god
// files would fail the gate on debt it did not add — a worse failure than the
// one this bead fixes, in the opposite direction. For "this FILE is too big",
// the path is the identity.
func TestRatchet_FileLevelFindingsStayPathKeyed(t *testing.T) {
	// A file-level finding: no node_id, therefore no content address.
	base := computeBaseline([]smellFinding{
		{RuleID: "god_file", SourceID: "internal/big.go", Line: 1},
	})
	require.Len(t, base.Counts, 1)
	assert.Empty(t, base.Counts[0].NodeHash,
		"a file-level finding must stay path-keyed, or every edit to the file re-flags it")

	// The file is edited (its content changed) but not moved: still grandfathered.
	assert.Empty(t, newDebt([]smellFinding{
		{RuleID: "god_file", SourceID: "internal/big.go", Line: 1},
	}, base), "editing a god file must not resurrect its own grandfathered finding")
}

// TestComputeBaseline_PooledKeyPicksSmallestSourceID pins determinism for the
// case content addressing creates: byte-identical debt in several files shares
// one key, so the entry's SourceID is a pointer to one of N files. Taking the
// first encountered would make the committed file depend on scan order.
func TestComputeBaseline_PooledKeyPicksSmallestSourceID(t *testing.T) {
	const same = "deadbeefdeadbeef"
	// Same content (same hash), three files, presented worst-case-first.
	findings := []smellFinding{
		{RuleID: "sleep_in_test", SourceID: "z/last.go", NodeHash: same, Line: 3},
		{RuleID: "sleep_in_test", SourceID: "a/first.go", NodeHash: same, Line: 1},
		{RuleID: "sleep_in_test", SourceID: "m/mid.go", NodeHash: same, Line: 2},
	}
	base := computeBaseline(findings)
	require.Len(t, base.Counts, 1, "identical content must pool into one key")
	assert.Equal(t, 3, base.Counts[0].Count, "the pooled allowance is the instance count")
	assert.Equal(t, "a/first.go", base.Counts[0].SourceID)

	// Any input order yields the same committed bytes.
	shuffled := []smellFinding{findings[1], findings[2], findings[0]}
	assert.Equal(t, base, computeBaseline(shuffled))
}
