package cmd

import (
	"archive/zip"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestOutFlag_DBSource verifies that --out works with .db data sources.
// This is a regression test: the .db branch in mount.go previously ignored
// --out and attempted to NFS mount, causing a hang.
func TestOutFlag_DBSource(t *testing.T) {
	// Create a minimal test .db with results table
	tmpDir := t.TempDir()
	srcDB := filepath.Join(tmpDir, "test.db")

	db, err := sql.Open("sqlite", srcDB)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL);
		INSERT INTO results VALUES ('a', '{"schema":"test","identifier":"item-1","item":{"name":"foo","value":"bar"}}');
		INSERT INTO results VALUES ('b', '{"schema":"test","identifier":"item-2","item":{"name":"baz","value":"qux"}}');
	`)
	require.NoError(t, err)
	_ = db.Close()

	outZip := filepath.Join(tmpDir, "out.zip")

	// Schema: simple flat projection
	schemaFile := filepath.Join(tmpDir, "schema.json")
	require.NoError(t, os.WriteFile(schemaFile, []byte(`{
		"version": "v1",
		"nodes": [{
			"name": "items",
			"selector": "$",
			"children": [{
				"name": "{{.item.name}}",
				"selector": "$[*]",
				"files": [{"name": "value", "content_template": "{{.item.value}}"}]
			}]
		}]
	}`), 0o644))

	// Save and restore global flags
	oldSchema := schemaPath
	oldData := dataPath
	oldOut := outPath
	oldFormat := outFormat
	defer func() {
		schemaPath = oldSchema
		dataPath = oldData
		outPath = oldOut
		outFormat = oldFormat
	}()

	schemaPath = schemaFile
	dataPath = srcDB
	outPath = outZip
	outFormat = "zip"

	// RunE should complete without hanging (the bug was a hang here)
	err = rootCmd.RunE(rootCmd, []string{filepath.Join(tmpDir, "mnt")})
	require.NoError(t, err)

	// Verify ZIP was created with content
	info, err := os.Stat(outZip)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))

	// Verify ZIP contains expected entries
	r, err := zip.OpenReader(outZip)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	assert.Contains(t, names, "items/foo/value")
	assert.Contains(t, names, "items/baz/value")
}
