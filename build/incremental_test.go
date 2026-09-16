package build

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/testfixtures"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dumpOf is the normalized projection dump, the same one the golden gate
// diffs — every row of nodes, node_refs, node_defs, file_index and
// _index_coverage, minus the per-build columns.
func dumpOf(t *testing.T, dbPath, corpus string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	dump, err := testfixtures.DumpProjection(db, corpus)
	require.NoError(t, err)
	return dump
}

// TestIncrementalBuild_ByteIdenticalToColdBuild is the correctness gate for
// mache-e7d9d0. Reusing a projection is only worth anything if the result is
// indistinguishable from building it from scratch.
//
// This is where the dedup-ordering hazard would surface: a rebuild that let a
// re-projected file claim a skipped file's ID, or left a deleted file's nodes
// behind, produces a dump that differs here even though every individual
// assertion about the changed file passes.
func TestIncrementalBuild_ByteIdenticalToColdBuild(t *testing.T) {
	corpus := testutil.GoldenCorpusDir(t)
	dir := t.TempDir()
	cold := filepath.Join(dir, "cold.db")
	warm := filepath.Join(dir, "warm.db")

	require.NoError(t, ParseWithSchemaRef(corpus, cold, "go", "."))
	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))
	// Second build into the same path: the fingerprint matches, so this one
	// reuses and re-projects only what changed — which is nothing.
	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))

	assert.Equal(t, dumpOf(t, cold, corpus), dumpOf(t, warm, corpus),
		"an incrementally rebuilt projection differs from a cold one")
}

// TestIncrementalBuild_FingerprintMismatchRebuilds: a projection written by a
// different SCHEMA must never be merged into. Reusing across schemas would
// leave the first schema's nodes sitting in the second's output.
func TestIncrementalBuild_FingerprintMismatchRebuilds(t *testing.T) {
	corpus := testutil.GoldenCorpusDir(t)
	out := filepath.Join(t.TempDir(), "out.db")

	require.NoError(t, ParseWithSchemaRef(corpus, out, "go", "."))
	withGo := dumpOf(t, out, corpus)
	require.NotEmpty(t, withGo)

	// Rebuild the same path with a different schema, then back again. If the
	// mismatch did not force a clean rebuild, the final dump would carry
	// remnants of the middle one.
	require.NoError(t, ParseWithSchemaRef(corpus, out, "markdown", "."))
	require.NoError(t, ParseWithSchemaRef(corpus, out, "go", "."))
	assert.Equal(t, withGo, dumpOf(t, out, corpus),
		"a schema change did not force a clean rebuild")
}

// TestReusableIndex_RequiresAMatchingFingerprint pins the reuse decision
// directly: no file, no fingerprint row, and a stale fingerprint each mean
// "project everything".
func TestReusableIndex_RequiresAMatchingFingerprint(t *testing.T) {
	corpus := testutil.GoldenCorpusDir(t)
	out := filepath.Join(t.TempDir(), "out.db")

	assert.Nil(t, reusableIndex(out, "anything"), "a missing output cannot be reused")

	require.NoError(t, ParseWithSchemaRef(corpus, out, "go", "."))
	got, err := readFingerprint(out)
	require.NoError(t, err)
	assert.NotEmpty(t, reusableIndex(out, got), "a matching fingerprint must be reusable")
	assert.Nil(t, reusableIndex(out, got+"-stale"), "a stale fingerprint must not be reused")

	// A db with no fingerprint row at all — anything written before this
	// change — is not reusable either.
	bare := filepath.Join(t.TempDir(), "bare.db")
	require.NoError(t, os.WriteFile(bare, []byte{}, 0o644))
	assert.Nil(t, reusableIndex(bare, got))
}

// copyCorpus stages a writable copy of the golden corpus, so a test can edit
// it without touching the committed fixture.
func copyCorpus(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if e.IsDir() {
			require.NoError(t, os.MkdirAll(to, 0o755))
			sub, serr := os.ReadDir(from)
			require.NoError(t, serr)
			for _, f := range sub {
				b, rerr := os.ReadFile(filepath.Join(from, f.Name()))
				require.NoError(t, rerr)
				require.NoError(t, os.WriteFile(filepath.Join(to, f.Name()), b, 0o644))
			}
			continue
		}
		b, rerr := os.ReadFile(from)
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(to, b, 0o644))
	}
	return dst
}

// TestIncrementalBuild_AfterAnEditMatchesAColdBuild is the strongest form of
// the correctness claim: incrementally rebuilding an EDITED tree must produce
// exactly what building that tree from scratch produces.
//
// Weaker assertions pass while the graph is wrong. Checking only that the
// edit landed misses a skipped file whose construct was overwritten; checking
// only the ID list misses a stale body. Comparing whole dumps catches both.
func TestIncrementalBuild_AfterAnEditMatchesAColdBuild(t *testing.T) {
	corpus := copyCorpus(t, testutil.GoldenCorpusDir(t))
	dir := t.TempDir()
	warm := filepath.Join(dir, "warm.db")

	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))

	// Edit one file, then rebuild incrementally.
	target := filepath.Join(corpus, "store", "index.go")
	original, err := os.ReadFile(target)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(target, append(original,
		[]byte("\nfunc AddedByTest() int { return 7 }\n")...), 0o644))
	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))

	// A cold build of the SAME edited tree is the reference.
	cold := filepath.Join(dir, "cold.db")
	require.NoError(t, ParseWithSchemaRef(corpus, cold, "go", "."))

	assert.Equal(t, dumpOf(t, cold, corpus), dumpOf(t, warm, corpus),
		"after an edit, the incrementally rebuilt projection differs from a cold one")
	assert.Contains(t, dumpOf(t, warm, corpus), "AddedByTest", "the edit never landed")
}

// TestIncrementalBuild_AfterADeleteMatchesAColdBuild: same claim for a file
// that disappears. Nothing walks a deleted file, so only the reap
// (mache-31abc0) keeps this equal.
func TestIncrementalBuild_AfterADeleteMatchesAColdBuild(t *testing.T) {
	corpus := copyCorpus(t, testutil.GoldenCorpusDir(t))
	dir := t.TempDir()
	warm := filepath.Join(dir, "warm.db")

	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))
	require.Contains(t, dumpOf(t, warm, corpus), "keyOf", "expected the corpus to define keyOf")

	require.NoError(t, os.Remove(filepath.Join(corpus, "store", "index.go")))
	require.NoError(t, ParseWithSchemaRef(corpus, warm, "go", "."))

	cold := filepath.Join(dir, "cold.db")
	require.NoError(t, ParseWithSchemaRef(corpus, cold, "go", "."))

	assert.Equal(t, dumpOf(t, cold, corpus), dumpOf(t, warm, corpus),
		"after a delete, the incrementally rebuilt projection differs from a cold one")
}
