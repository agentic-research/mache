package build_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/build"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishFixture writes a one-function Go tree and builds it once, so each
// test starts with a published projection at output.
func publishFixture(t *testing.T) (dir, source, output string) {
	t.Helper()
	dir = t.TempDir()
	source = filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(source, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.go"), []byte(`package sample

func Use() string { return "ok" }
`), 0o644))
	output = filepath.Join(dir, "projected.db")
	require.NoError(t, build.ParseWithSchemaRef(source, output, "go", source))
	return dir, source, output
}

// A build that fails after projecting must leave the previous database exactly
// as it was. In-place projection could not: a full rebuild removed output
// before it started, and an incremental one had already rewritten rows.
func TestParseWithSchema_AFailedBuildLeavesThePreviousOutput(t *testing.T) {
	dir, source, output := publishFixture(t)
	before, err := os.ReadFile(output)
	require.NoError(t, err)

	// Change the source so the failed build had something new to write.
	require.NoError(t, os.WriteFile(filepath.Join(source, "extra.go"), []byte(`package sample

func Extra() int { return 1 }
`), 0o644))
	boom := errors.New("boom")
	err = build.ParseWithSchemaRef(source, output, "go", source,
		build.WithFinalize(func(string) error { return boom }))
	require.ErrorIs(t, err, boom)

	after, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a failed build modified the published database")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"src", "projected.db"}, names, "the build left its work behind")
}

// Finalize runs against the finished projection on its private path, before
// publish: what it writes is part of the published database, and output is not
// touched until it returns.
func TestParseWithSchema_FinalizeSeesTheProjectionBeforePublish(t *testing.T) {
	_, source, output := publishFixture(t)
	before, err := os.ReadFile(output)
	require.NoError(t, err)

	var finalizedAt string
	require.NoError(t, build.ParseWithSchemaRef(source, output, "go", source,
		build.WithFinalize(func(path string) error {
			finalizedAt = path
			require.Equal(t, 1, sqliteCount(t, path,
				`SELECT count(*) FROM nodes WHERE id = 'sample/functions/Use/source'`),
				"finalize ran before the projection was complete")
			still, err := os.ReadFile(output)
			require.NoError(t, err)
			require.Equal(t, before, still, "output changed before finalize returned")

			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			_, err = db.Exec(`INSERT INTO _mache_meta (key, value) VALUES ('stamped', 'yes')`)
			return err
		})))

	assert.NotEqual(t, output, finalizedAt, "finalize was handed the live output path")
	assert.Equal(t, 1, sqliteCount(t, output,
		`SELECT count(*) FROM _mache_meta WHERE key = 'stamped' AND value = 'yes'`),
		"what finalize wrote is missing from the published database")
}
