package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testutil"
)

// treeDB parses, with the pinned leyline, a tree holding a parsed file, a file
// that parsed with errors, a binary, a text file no parser takes, and a
// directory holding only a binary, and serves the db as `mache serve` does.
func treeDB(t *testing.T) graph.Graph {
	t.Helper()
	src := t.TempDir()
	for name, body := range map[string]string{
		"pkg/a.go":        "package pkg\n\nfunc A() int { return 1 }\n",
		"pkg/bad.go":      "package pkg\n\nfunc broken( {\n",
		"blob.bin":        "\x00\x01bin",
		"notes.txt":       "notes\n",
		"assets/logo.bin": "\x89PNG",
	} {
		p := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	var seq int
	var name, file string
	require.NoError(t, db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file))
	g, _, cleanup, err := openDBGraph(file, &api.Topology{Version: api.SchemaVersion}, graph.OpenSQLiteGraph, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return g
}

func listDir(t *testing.T, g graph.Graph, path string) map[string]nodeEntry {
	t.Helper()
	res, err := makeListDirHandler(g)(context.Background(), testutil.MakeRequest(map[string]any{"path": path}))
	require.NoError(t, err)
	require.False(t, res.IsError, testutil.ResultText(t, res))
	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &entries))
	byName := map[string]nodeEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	return byName
}

// TestListDirectory_ShowsWhatNoParserProjected is mache-3a375a's decision:
// _tree is the authority on what exists. A file no parser projected is listed,
// marked unparsed, and a directory holding only such files lists too; a parsed
// file is listed as before and carries the parse's diagnostic when it has one.
func TestListDirectory_ShowsWhatNoParserProjected(t *testing.T) {
	g := treeDB(t)

	root := listDir(t, g, "")
	for _, name := range []string{"blob.bin", "notes.txt"} {
		require.Contains(t, root, name)
		assert.True(t, root[name].Unparsed, name)
		assert.Equal(t, "file", root[name].Type, name)
	}
	assert.Equal(t, int64(6), root["notes.txt"].Size)
	require.Contains(t, root, "assets")
	assert.Equal(t, "dir", root["assets"].Type)
	require.Contains(t, root, "pkg")
	assert.False(t, root["pkg"].Unparsed)

	assets := listDir(t, g, "assets")
	require.Contains(t, assets, "logo.bin", "a directory only _tree holds lists from it")
	assert.True(t, assets["logo.bin"].Unparsed)

	pkg := listDir(t, g, "pkg")
	require.Contains(t, pkg, "a.go")
	assert.False(t, pkg["a.go"].Unparsed)
	assert.Empty(t, pkg["a.go"].Diagnostic)
	require.Contains(t, pkg, "bad.go")
	assert.False(t, pkg["bad.go"].Unparsed, "a file that parsed with errors is projected")
	assert.Contains(t, pkg["bad.go"].Diagnostic, "syntax error")
}

// TestReadFile_ReadsAnUnparsedTextFile: an unparsed file's bytes are stored,
// so a text one reads; a binary one is refused with what it is.
func TestReadFile_ReadsAnUnparsedTextFile(t *testing.T) {
	g := treeDB(t)

	r, err := readOneFileWithOrigin(g, "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "notes\n", r.Content)

	_, err = readOneFileWithOrigin(g, "blob.bin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary file (5 bytes)")
}
