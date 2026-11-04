package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/agentic-research/mache/internal/writeback"
)

// otherWriter changes srcPath through the daemon serving its tree, as another
// client would, against the daemon's current copy.
func otherWriter(t *testing.T, srcPath string, edit func(string) string) {
	t.Helper()
	dir := filepath.Dir(srcPath)
	c, err := leyline.WritableDaemon(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	read, err := c.ReadSource(filepath.Base(srcPath))
	require.NoError(t, err)
	res, err := c.Propose([]leyline.ProposedWrite{{Path: filepath.Base(srcPath), Base: read.BlobHash, Content: proposedText(edit(read.Content))}})
	require.NoError(t, err)
	require.Nil(t, res.Refusal, res.Message)
}

type writeResp struct {
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	GraphWarning string `json:"graph_warning"`
	Conflicts    []struct {
		StartLine uint32 `json:"start_line"`
		EndLine   uint32 `json:"end_line"`
	} `json:"conflicts"`
}

func writeNode(t *testing.T, g graph.Graph, path, content string) (writeResp, string) {
	t.Helper()
	result, err := makeWriteFileHandler(g)(context.Background(), testutil.MakeRequest(map[string]any{
		"path": path, "content": content, "format": false,
	}))
	require.NoError(t, err)
	text := testutil.ResultText(t, result)
	var r writeResp
	if !result.IsError {
		require.NoError(t, json.Unmarshal([]byte(text), &r))
	}
	return r, text
}

// TestWriteFile_RefusedWhenTheNodeMovedUnderIt is mache-3a0fb9's point: a
// write is proposed against the bytes the node was read from, so a change
// another writer made to the same lines since is not overwritten. The write
// is refused stale with the overlapping lines, and the file keeps the other
// writer's change.
func TestWriteFile_RefusedWhenTheNodeMovedUnderIt(t *testing.T) {
	store, nodePath, srcPath := buildWriteGraph(t)
	otherWriter(t, srcPath, func(s string) string { return s[:len(s)-len("\treturn\n}\n")] + "\treturn // other\n}\n" })

	r, _ := writeNode(t, store, nodePath, "func Hello() {\n\treturn // mine\n}\n")

	assert.Equal(t, "write_refused", r.Status)
	assert.Equal(t, "stale", r.Reason)
	require.NotEmpty(t, r.Conflicts)
	assert.Equal(t, uint32(4), r.Conflicts[0].StartLine, "the return line, 1-based")
	got, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "// other")
	assert.NotContains(t, string(got), "// mine")
}

// TestWriteFile_MergesAChangeElsewhereThenAsksForAReRead: a change another
// writer made to other lines is merged with the write; the file then holds
// both, which is not what mache built, so the next write is refused until the
// node is re-read rather than applied at offsets that no longer fit.
func TestWriteFile_MergesAChangeElsewhereThenAsksForAReRead(t *testing.T) {
	store, nodePath, srcPath := buildWriteGraph(t)
	hello, err := store.GetNode(nodePath)
	require.NoError(t, err)
	// A second node in the same file: after the merge its offsets no longer
	// fit the file either, so it must stop naming a base too.
	clause := &graph.SourceOrigin{FilePath: srcPath, StartByte: 0, EndByte: 12, Blob: hello.Origin.Blob}
	store.AddNode(&graph.Node{ID: "pkg/clause", Origin: clause, Data: []byte("package main")})
	otherWriter(t, srcPath, func(s string) string { return "// header\n" + s })

	r, _ := writeNode(t, store, nodePath, "func Hello() {\n\treturn // mine\n}\n")

	assert.Equal(t, "ok_merged", r.Status)
	assert.Contains(t, r.GraphWarning, "re-read")
	got, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "// header")
	assert.Contains(t, string(got), "// mine")

	for _, p := range []string{"pkg/clause", nodePath} {
		again, text := writeNode(t, store, p, "package main // again")
		assert.Empty(t, again.Status, "%s: refused before any proposal", p)
		assert.Contains(t, text, writeback.ErrNoBase.Error(), "%s names no base after the merge", p)
	}
	got, err = os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.NotContains(t, string(got), "// again")
}

// TestWriteFile_ThroughAnAdoptedDaemonOnlyOnItsTree: a daemon mache did not
// start (reached by LEYLINE_SOCKET) says which tree it serves in status, so a
// write goes through it when that is the node's tree, and is refused, the file
// untouched, when it serves another (ley-line-open-246526).
func TestWriteFile_ThroughAnAdoptedDaemonOnlyOnItsTree(t *testing.T) {
	store, nodePath, srcPath := buildWriteGraph(t)
	before, err := os.ReadFile(srcPath)
	require.NoError(t, err)

	t.Setenv("LEYLINE_SOCKET", lltest.StartPinnedSourceDaemon(t, t.TempDir()))
	_, text := writeNode(t, store, nodePath, "func Hello() {\n\treturn // elsewhere\n}\n")
	assert.Contains(t, text, "serves", "refused, naming the tree the daemon serves")
	after, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	t.Setenv("LEYLINE_SOCKET", lltest.StartPinnedSourceDaemon(t, filepath.Dir(srcPath)))
	r, text := writeNode(t, store, nodePath, "func Hello() {\n\treturn // mine\n}\n")
	require.Equal(t, "ok", r.Status, text)
	after, err = os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Contains(t, string(after), "// mine", "the adopted daemon wrote the file")
}

// proposedText is a proposed write's content, which the wire carries as a
// pointer.
func proposedText(s string) *string { return &s }
