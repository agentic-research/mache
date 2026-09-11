package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStageHEAD_PinsTheCorpus is the claim the whole bench rests on: two runs
// are only comparable if they measured the same bytes. Staging from
// `git archive HEAD` means the corpus is the commit, not the working tree —
// an editor scratch file or a half-finished edit must not silently become
// part of what was benchmarked.
func TestStageHEAD_PinsTheCorpus(t *testing.T) {
	root, err := macheRepoRoot()
	require.NoError(t, err)

	// A file in the working tree that is not in HEAD.
	scratch := filepath.Join(root, "bench-cold-staging-probe.txt")
	require.NoError(t, os.WriteFile(scratch, []byte("not committed"), 0o644))
	t.Cleanup(func() { _ = os.Remove(scratch) })

	dir := t.TempDir()
	sha, err := stageHEAD(root, dir)
	require.NoError(t, err)
	assert.Len(t, sha, 40, "the corpus is identified by the full commit SHA")

	_, err = os.Stat(filepath.Join(dir, "bench-cold-staging-probe.txt"))
	assert.True(t, os.IsNotExist(err),
		"an uncommitted working-tree file reached the staged corpus; the bench would be measuring bytes no other run can reproduce")

	// A tracked file must be there, or the staging silently produced nothing
	// and every number would describe an empty corpus.
	_, err = os.Stat(filepath.Join(dir, "go.mod"))
	assert.NoError(t, err, "tracked files must reach the staged corpus")
}

// TestCorpusSize_CountsRegularFiles: every wall-time number is reported next
// to the size of what produced it, so the count has to be real.
func TestCorpusSize_CountsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), make([]byte, 10), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b.go"), make([]byte, 32), 0o644))

	files, bytes, err := corpusSize(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, files, "directories are not files")
	assert.Equal(t, int64(42), bytes)
}

func TestShortSHA(t *testing.T) {
	assert.Equal(t, "1203ab2", shortSHA("1203ab2fd8703cf11657d57445f65bc7a3225ec8"))
	assert.Equal(t, "-", shortSHA(""))
	assert.Equal(t, "abc", shortSHA("abc"))
}
