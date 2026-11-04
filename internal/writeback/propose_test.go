package writeback

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// Issue #6: an agent writing through echo/heredoc adds a trailing newline the
// original range did not have; keeping it leaves a blank-line artifact.
func TestReplaceRange_DropsATrailingNewlineTheRangeDidNotHave(t *testing.T) {
	base := []byte("package main\n\nconst maxRetries = 3\n\nfunc main() {}\n")
	origin := graph.SourceOrigin{StartByte: 14, EndByte: 34} // "const maxRetries = 3"

	file, content, err := replaceRange(base, origin, []byte("const maxRetries = 5\n"))

	require.NoError(t, err)
	assert.Equal(t, "package main\n\nconst maxRetries = 5\n\nfunc main() {}\n", string(file))
	assert.Equal(t, "const maxRetries = 5", string(content), "the node holds what went into the file")
}

func TestReplaceRange_KeepsATrailingNewlineTheRangeHad(t *testing.T) {
	base := []byte("func A() {}\nfunc B() {}\nfunc C() {}\n")
	origin := graph.SourceOrigin{StartByte: 12, EndByte: 24} // "func B() {}\n"

	file, _, err := replaceRange(base, origin, []byte("func B() { return 1 }\n"))

	require.NoError(t, err)
	assert.Equal(t, "func A() {}\nfunc B() { return 1 }\nfunc C() {}\n", string(file))
}

func TestReplaceRange_RefusesARangeOutsideTheBase(t *testing.T) {
	for _, o := range []graph.SourceOrigin{{StartByte: 0, EndByte: 99}, {StartByte: 5, EndByte: 2}} {
		_, _, err := replaceRange([]byte("short"), o, []byte("x"))
		assert.Error(t, err, "%+v", o)
	}
}

// fixedBlobs is a BlobSource holding one blob.
type fixedBlobs struct{ hash, content []byte }

func (f fixedBlobs) Blob(h []byte) ([]byte, bool) {
	if string(h) == string(f.hash) {
		return f.content, true
	}
	return nil, false
}
func (fixedBlobs) Remember(string, []byte, []byte) {}

// TestPropose_RefusesWithoutABase: without the bytes a node was read from,
// mache cannot build the file its offsets describe, so it does not write. No
// daemon is asked, and nothing reads the file on disk instead.
func TestPropose_RefusesWithoutABase(t *testing.T) {
	origin := graph.SourceOrigin{FilePath: "/x/a.go", StartByte: 0, EndByte: 1, Blob: []byte{1}}
	for name, tc := range map[string]struct {
		origin graph.SourceOrigin
		blobs  graph.BlobSource
	}{
		"no blob source":       {origin, nil},
		"origin names no blob": {graph.SourceOrigin{FilePath: "/x/a.go"}, fixedBlobs{[]byte{1}, []byte("a")}},
		"blob not held":        {origin, fixedBlobs{[]byte{2}, []byte("a")}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Propose(tc.origin, []byte("b"), tc.blobs)
			assert.ErrorIs(t, err, ErrNoBase)
		})
	}
}

// TestRelativeTo_RefusesAPathOutsideTheTree: the daemon resolves a propose's
// path under its --source, so a file outside it cannot be written through it.
func TestRelativeTo_RefusesAPathOutsideTheTree(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "pkg", "a.go") // a written node's file exists
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, os.WriteFile(file, []byte("package pkg\n"), 0o644))
	rel, err := relativeTo(root, file)
	require.NoError(t, err)
	assert.Equal(t, "pkg/a.go", rel)

	for _, p := range []string{filepath.Join(filepath.Dir(root), "elsewhere.go"), "/etc/hosts"} {
		_, err := relativeTo(root, p)
		assert.ErrorIs(t, err, leyline.ErrNoWritableDaemon, p)
	}
	_, err = relativeTo("", filepath.Join(root, "a.go"))
	assert.ErrorIs(t, err, leyline.ErrNoWritableDaemon, "no daemon source configured")
}
