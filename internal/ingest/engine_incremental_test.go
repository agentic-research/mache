package ingest

import (
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildInto projects dir into dbPath through a SQLiteWriter, the way mount
// does. index is nil for a full build; for an incremental pass it is whatever
// LoadFileIndex recovered from the previous one.
func buildInto(t *testing.T, dir, dbPath string, index map[string]FileIndexEntry) {
	t.Helper()
	writer, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)
	engine := NewEngine(fnPerConstructSchema(), writer)
	if index != nil {
		engine.SetFileIndex(index)
	}
	attachLeylineAST(t, engine, dir)
	require.NoError(t, engine.Ingest(dir))
	require.NoError(t, writer.Close())
}

// constructs lists the construct IDs under fns/ in the built db, with the
// source text each one holds — identity AND content, because the bug replaces
// one file's body with another's while the ID stays put.
func constructs(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT parent_id, record FROM nodes
		WHERE name = 'source' AND parent_id LIKE 'fns/%' ORDER BY parent_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var id string
		var record sql.NullString
		require.NoError(t, rows.Scan(&id, &record))
		out[id] = record.String
	}
	require.NoError(t, rows.Err())
	return out
}

// writeSource makes path look changed to the (mtime, size) index without altering
// what it declares, so the incremental pass re-projects exactly that file.
func writeSource(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// TestIncremental_ChangedFileCannotStealASkippedFileID pins mache-7a7919,
// end to end through the path mount uses: build, recover the file index from
// the db, re-project into the SAME db with one file unchanged.
//
// a.rs and b.rs both declare `shared`. The full build gives a.rs the bare ID
// and b.rs the suffix — lexical order over the full path, pinned elsewhere.
// Before the fix, an incremental pass that skipped a.rs let the changed b.rs
// claim `fns/shared`, and that write REPLACED a.rs's function with b.rs's
// body while `fns/shared.from_b_rs` was left pointing at content that was no
// longer anywhere.
func TestIncremental_ChangedFileCannotStealASkippedFileID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	a := filepath.Join(dir, "a.rs")
	b := filepath.Join(dir, "b.rs")
	writeSource(t, a, "pub fn shared() -> u8 { 1 }\n")
	writeSource(t, b, "pub fn shared() -> u8 { 2 }\n")

	buildInto(t, dir, dbPath, nil)
	full := constructs(t, dbPath)
	require.Equal(t, []string{"fns/shared", "fns/shared.from_b_rs"}, slices.Sorted(maps.Keys(full)))
	require.Contains(t, full["fns/shared"], "{ 1 }", "a.rs owns the bare ID on a full build")

	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.NotEmpty(t, index)

	// b.rs changes; a.rs does not.
	writeSource(t, b, "pub fn shared() -> u8 { 22 }\n")
	buildInto(t, dir, dbPath, index)

	incr := constructs(t, dbPath)
	assert.Equal(t, slices.Sorted(maps.Keys(full)), slices.Sorted(maps.Keys(incr)),
		"an incremental pass produced different IDs than a full build")
	assert.Contains(t, incr["fns/shared"], "{ 1 }",
		"fns/shared holds the CHANGED file's body — a.rs's construct was replaced")
	assert.Contains(t, incr["fns/shared.from_b_rs"], "{ 22 }",
		"the changed file's edit did not land")
}

// TestIncremental_SkippedFileSortsLast: the skipped file is the one holding
// the SUFFIX, so a fix that seeds from the first file by accident fails here.
func TestIncremental_SkippedFileSortsLast(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	a := filepath.Join(dir, "a.rs")
	b := filepath.Join(dir, "b.rs")
	writeSource(t, a, "pub fn shared() -> u8 { 1 }\n")
	writeSource(t, b, "pub fn shared() -> u8 { 2 }\n")

	buildInto(t, dir, dbPath, nil)
	full := constructs(t, dbPath)
	require.Contains(t, full["fns/shared.from_b_rs"], "{ 2 }")

	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)

	// This time a.rs changes and b.rs — which holds the suffix — is skipped.
	writeSource(t, a, "pub fn shared() -> u8 { 11 }\n")
	buildInto(t, dir, dbPath, index)

	incr := constructs(t, dbPath)
	assert.Equal(t, slices.Sorted(maps.Keys(full)), slices.Sorted(maps.Keys(incr)))
	assert.Contains(t, incr["fns/shared"], "{ 11 }", "the changed file's edit did not land")
	assert.Contains(t, incr["fns/shared.from_b_rs"], "{ 2 }",
		"the skipped file's construct was overwritten")
}

// TestLoadFileIndex_RecoversClaimedIDs is the seam the fix rests on: the
// previous build's claims have to be recoverable at all. A construct
// directory carries no source_file — only its leaves do — so the owner is
// recovered through the leaf's parent.
func TestLoadFileIndex_RecoversClaimedIDs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	writeSource(t, filepath.Join(dir, "a.rs"), "pub fn alpha() -> u8 { 1 }\npub fn second() -> u8 { 2 }\n")
	buildInto(t, dir, dbPath, nil)

	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.Len(t, index, 1)
	for path, entry := range index {
		assert.ElementsMatch(t, []string{"fns/alpha", "fns/second"}, entry.ClaimedIDs,
			"claims for %s", path)
	}
}

// defsFor lists the def tokens the built db holds for constructs under fns/.
// Asserted separately from the node tree because dead_code and
// duplicate_definitions judge node_defs, not ListChildren — a reap that left
// these behind would still inflate every rule that counts definitions.
func defsFor(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT token FROM node_defs WHERE node_id LIKE 'fns/%' ORDER BY token`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var token string
		require.NoError(t, rows.Scan(&token))
		out = append(out, token)
	}
	require.NoError(t, rows.Err())
	return out
}

// indexedPaths is what the db believes the last build projected.
func indexedPaths(t *testing.T, dbPath string) []string {
	t.Helper()
	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	return slices.Sorted(maps.Keys(index))
}

// TestIncremental_DeletedFileIsReaped pins mache-31abc0.
//
// An incremental pass walks the files that are PRESENT, so a deleted file is
// never visited and nothing removed what it projected. The graph kept serving
// a function that was no longer in the source.
func TestIncremental_DeletedFileIsReaped(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	a := filepath.Join(dir, "a.rs")
	b := filepath.Join(dir, "b.rs")
	writeSource(t, a, "pub fn alpha() -> u8 { 1 }\n")
	writeSource(t, b, "pub fn beta() -> u8 { 2 }\n")

	buildInto(t, dir, dbPath, nil)
	require.Equal(t, []string{"fns/alpha", "fns/beta"}, slices.Sorted(maps.Keys(constructs(t, dbPath))))
	require.Equal(t, []string{"alpha", "beta"}, defsFor(t, dbPath))
	require.Len(t, indexedPaths(t, dbPath), 2)

	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(b))
	buildInto(t, dir, dbPath, index)

	after := constructs(t, dbPath)
	assert.Equal(t, []string{"fns/alpha"}, slices.Sorted(maps.Keys(after)),
		"the deleted file's construct survived the re-index")
	assert.Contains(t, after["fns/alpha"], "{ 1 }", "the surviving file's construct changed")
	assert.Equal(t, []string{"alpha"}, defsFor(t, dbPath),
		"node_defs still holds the deleted function; dead_code would keep counting it")

	// The index row goes too, or the path stays known forever and every later
	// build repeats the decision.
	assert.Len(t, indexedPaths(t, dbPath), 1, "the deleted path is still in file_index")
}

// TestIncremental_DeletedFileOwnedTheBareName: when the DELETED file held the
// bare ID, the survivor takes it — exactly as a cold build of the remaining
// tree would.
//
// This REVERSES what this test asserted when it was written (mache-31abc0),
// and the reversal is deliberate. The original reasoning was reference
// stability: renaming a construct breaks any ID an agent already holds. The
// stronger property won instead — an incrementally rebuilt projection must be
// byte-identical to a cold one (mache-e7d9d0), because otherwise two people
// with identical source get different graphs depending on their edit history,
// and the golden gate cannot pin the projection as a function of the source.
//
// You cannot have both once the bare name frees up. Determinism is the one
// worth keeping, and it is the one a gate can enforce.
func TestIncremental_DeletedFileOwnedTheBareName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	a := filepath.Join(dir, "a.rs")
	b := filepath.Join(dir, "b.rs")
	writeSource(t, a, "pub fn shared() -> u8 { 1 }\n")
	writeSource(t, b, "pub fn shared() -> u8 { 2 }\n")

	buildInto(t, dir, dbPath, nil)
	require.Contains(t, constructs(t, dbPath)["fns/shared"], "{ 1 }", "a.rs owns the bare ID")

	index, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(a)) // the owner of the bare name is deleted
	buildInto(t, dir, dbPath, index)

	after := constructs(t, dbPath)
	assert.Equal(t, []string{"fns/shared"}, slices.Sorted(maps.Keys(after)),
		"the survivor should hold the bare ID, as a cold build of b.rs alone would")
	assert.Contains(t, after["fns/shared"], "{ 2 }", "the surviving construct is b.rs's")
}

// TestIncremental_FullBuildReapsNothing: with no file index there is nothing
// claiming to know what the last build projected, so the reap must not run.
func TestIncremental_FullBuildReapsNothing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	writeSource(t, filepath.Join(dir, "a.rs"), "pub fn alpha() -> u8 { 1 }\n")

	buildInto(t, dir, dbPath, nil)
	buildInto(t, dir, dbPath, nil) // again, still no index
	assert.Equal(t, []string{"fns/alpha"}, slices.Sorted(maps.Keys(constructs(t, dbPath))))
}
