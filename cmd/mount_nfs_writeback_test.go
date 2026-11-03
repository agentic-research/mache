package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
)

// When the validator cannot answer (the leyline daemon is unreachable or too
// old), a write must fail, not draft. Drafting reported success to the writer
// while the source file, and so every build, kept the old code; only a virtual
// _diagnostics file said otherwise (mache-3a213e).
func TestSpliceWriteBack_UnreachableValidatorRefusesTheWrite(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "dead.sock")
	require.NoError(t, os.WriteFile(dead, nil, 0o600))
	t.Setenv("LEYLINE_SOCKET", dead) // a path that is not a socket: no daemon answers

	src := filepath.Join(t.TempDir(), "a.go")
	original := []byte("package a\n\nfunc A() int { return 1 }\n")
	require.NoError(t, os.WriteFile(src, original, 0o644))
	fn := "package a\n\nfunc A() int { return 1 }"
	store := graph.NewMemoryStore()
	origin := &graph.SourceOrigin{FilePath: src, StartByte: 11, EndByte: uint32(len(fn))}
	store.AddNode(&graph.Node{ID: "a/functions/A/source", Data: original[11:len(fn)], Origin: origin})

	err := spliceWriteBack(store)("a/functions/A/source", *origin, []byte("func A() int { return 2 }"))
	require.Error(t, err, "the writer is told the edit did not land")
	assert.Contains(t, err.Error(), "validator unavailable")

	onDisk, rerr := os.ReadFile(src)
	require.NoError(t, rerr)
	assert.Equal(t, original, onDisk, "the source file is untouched")
	n, gerr := store.GetNode("a/functions/A/source")
	require.NoError(t, gerr)
	assert.Nil(t, n.DraftData, "no draft stands in for the refused edit")
	buf := make([]byte, n.ContentSize())
	k, rerr := store.ReadContent("a/functions/A/source", buf, 0)
	require.NoError(t, rerr)
	assert.Equal(t, "func A() int { return 1 }", string(buf[:k]), "the mount serves the source bytes")
	status, _ := store.WriteStatus.Load("a/functions/A")
	assert.Contains(t, status, "validator unavailable", "_diagnostics says why")
}
