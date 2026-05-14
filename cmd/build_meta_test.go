package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestWriteBuildMetadata_RoundTrips pins issue #1 in
// claude/fix-mache-schema-bugs-6vBkF: every `mache build` stamps a
// `_mache_meta` table so consumers can distinguish leyline-produced
// .dbs from in-process tree-sitter ones without `.tables` archaeology.
func TestWriteBuildMetadata_RoundTrips(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Pre-create the .db (writeBuildMetadata expects the file to exist
	// — both build paths produce it before stamping).
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, writeBuildMetadata(dbPath, "tree-sitter"))

	db, err = sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT key, value FROM _mache_meta`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	got := map[string]string{}
	for rows.Next() {
		var k, v string
		require.NoError(t, rows.Scan(&k, &v))
		got[k] = v
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, "tree-sitter", got["backend"], "backend column must record the dispatch path")
	assert.NotEmpty(t, got["mache_version"], "mache_version must be stamped (even 'dev')")
	assert.NotEmpty(t, got["built_at"], "built_at must be stamped")
}

// TestWriteBuildMetadata_OverwritesOnRebuild pins that a rebuild of
// the same .db replaces the marker rather than appending — otherwise
// a `key` PK collision would error and the build would log a warning
// on every subsequent build.
func TestWriteBuildMetadata_OverwritesOnRebuild(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, writeBuildMetadata(dbPath, "tree-sitter"))
	require.NoError(t, writeBuildMetadata(dbPath, "leyline"))

	db, err = sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var backend string
	require.NoError(t, db.QueryRow(
		`SELECT value FROM _mache_meta WHERE key = 'backend'`,
	).Scan(&backend))
	assert.Equal(t, "leyline", backend, "second build's backend must overwrite the first")
}

// TestCountSourceFiles_SkipsHiddenAndVendor pins that the empty-build
// warning's denominator doesn't count files in `.git` / `vendor` /
// `node_modules` — those would inflate the count and produce a false
// "you have N files but produced 0 nodes" warning even when the build
// correctly ignored those dirs.
func TestCountSourceFiles_SkipsHiddenAndVendor(t *testing.T) {
	src := t.TempDir()
	// Real source.
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\n"), 0o644))
	// Skipped dirs should NOT contribute.
	require.NoError(t, os.MkdirAll(filepath.Join(src, "vendor", "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "vendor", "pkg", "lib.go"),
		[]byte("package pkg\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "HEAD.go"),
		[]byte("package fake\n"), 0o644))

	got := countSourceFiles(src)
	assert.Equal(t, 1, got, "vendor/ and .git/ entries must be skipped")
}

// TestCountSourceFiles_OnlyRecognizedExts pins that the count only
// includes extensions lang.IsSourceExt recognizes — a tree of .txt
// files shouldn't trigger a false "0 nodes produced" warning when
// the build correctly produced 0 nodes (because there was nothing
// to parse).
func TestCountSourceFiles_OnlyRecognizedExts(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "notes.txt"),
		[]byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "data.bin"),
		[]byte{0, 1, 2}, 0o644))

	got := countSourceFiles(src)
	assert.Equal(t, 0, got, "unrecognized extensions must not contribute to the source count")
}

// TestCountNodes_MissingTableReturnsNegative pins that we don't
// accidentally treat "no nodes table" as "0 nodes" — only "nodes
// table present and empty" should fire the empty-build warning.
// Otherwise a non-mache .db handed to readBuildBackend would trigger
// a confusing warning about parser support.
func TestCountNodes_MissingTableReturnsNegative(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	got := countNodes(dbPath)
	assert.Equal(t, -1, got, "missing nodes table must return -1, not 0")
}
