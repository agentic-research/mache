package cmd

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
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
	require.NoError(t, db.Close())

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

// TestOutFlag_JSONSource — #820, mache-eb0278: --out with plain JSON data
// was ignored and mache went on to NFS-mount. It must write the projection
// and return without mounting.
func TestOutFlag_JSONSource(t *testing.T) {
	tmpDir := t.TempDir()
	srcJSON := filepath.Join(tmpDir, "data.json")
	require.NoError(t, os.WriteFile(srcJSON,
		[]byte(`[{"name":"foo","value":"bar"},{"name":"baz","value":"qux"}]`), 0o644))
	schemaFile := filepath.Join(tmpDir, "schema.json")
	require.NoError(t, os.WriteFile(schemaFile, []byte(`{
		"version": "v1",
		"nodes": [{
			"name": "items",
			"selector": "$",
			"children": [{
				"name": "{{.name}}",
				"selector": "$[*]",
				"files": [{"name": "value", "content_template": "{{.value}}"}]
			}]
		}]
	}`), 0o644))
	outJSON := filepath.Join(tmpDir, "out.json")

	oldSchema, oldData, oldOut, oldFormat, oldMount := schemaPath, dataPath, outPath, outFormat, mountGraph
	defer func() {
		schemaPath, dataPath, outPath, outFormat, mountGraph = oldSchema, oldData, oldOut, oldFormat, oldMount
	}()
	mountGraph = func(*api.Topology, graph.Graph, *ingest.Engine, string, bool, []byte) error {
		t.Fatal("--out must not mount")
		return nil
	}
	schemaPath, dataPath, outPath, outFormat = schemaFile, srcJSON, outJSON, "json"

	require.NoError(t, rootCmd.RunE(rootCmd, nil))

	exported, err := os.ReadFile(outJSON)
	require.NoError(t, err)
	var entries []struct {
		Name     string
		Children json.RawMessage
	}
	require.NoError(t, json.Unmarshal(exported, &entries))
	for _, entry := range entries {
		if entry.Name == "items" {
			assert.JSONEq(t, `[
				{"name":"baz","type":"directory","children":[{"name":"value","type":"file","size":3,"content":"qux"}]},
				{"name":"foo","type":"directory","children":[{"name":"value","type":"file","size":3,"content":"bar"}]}
			]`, string(entry.Children))
			return
		}
	}
	t.Fatal("export does not contain the items directory")
}

// mache-09e85f: `mache --out x.db ./src` read ./src as a mountpoint and
// exported the default data path. With no --data, a positional argument is
// refused with the command to run; with --data it is the placeholder
// mountpoint older docs required, and is tolerated.
func TestOutFlag_PositionalArgument(t *testing.T) {
	oldData, oldOut := dataPath, outPath
	defer func() { dataPath, outPath = oldData, oldOut }()
	outPath = "out.db"

	dataPath = ""
	err := checkOutArgs([]string{"./src"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mache --data ./src --out out.db")

	dataPath = "data.json"
	assert.NoError(t, checkOutArgs([]string{"/tmp/unused"}))
	assert.NoError(t, checkOutArgs(nil))

	outPath = ""
	dataPath = ""
	assert.NoError(t, checkOutArgs([]string{"/mnt"}), "a mount is not an export")
}
