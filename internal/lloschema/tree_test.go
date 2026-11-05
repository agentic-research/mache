package lloschema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/agentic-research/mache/internal/lltest"
)

// TestTreeChildren_IsADirectorysDirectChildren, on a real pinned-leyline parse:
// direct children only, parsed or not, with their diagnostics. Siblings that
// LIKE 'a_b/%' would match (a _ or % where the name has a letter) and a
// sibling that extends the directory's name are not mistaken for children.
// (A case-only twin cannot exist on a case-insensitive filesystem, so it is
// not exercised here.)
func TestTreeChildren_IsADirectorysDirectChildren(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{
		"a_b/x.go":   "package x\n",
		"aXb/y.txt":  "y\n",
		"a%b/z.txt":  "z\n",
		"a_b2/v.txt": "v\n",
		"a_b/bad.go": "package x\n\nfunc broken( {\n",
		"a_b/deep/d": "d\n",
		"top.bin":    "\x00",
	} {
		p := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	qg := lloschema.DB(db)
	has, err := lloschema.HasTree(qg)
	require.NoError(t, err)
	require.True(t, has, "projection-v7 records its tree")

	kids, err := lloschema.TreeChildren(qg, "a_b")
	require.NoError(t, err)
	byName := map[string]lloschema.TreeEntry{}
	for _, k := range kids {
		byName[k.Name] = k
	}
	assert.ElementsMatch(t, []string{"x.go", "bad.go", "deep"}, keys(byName))
	assert.True(t, byName["x.go"].Parsed)
	assert.Empty(t, byName["x.go"].Diagnostic)
	assert.Contains(t, byName["bad.go"].Diagnostic, "syntax error")
	assert.True(t, byName["deep"].IsDir)

	root, err := lloschema.TreeChildren(qg, "")
	require.NoError(t, err)
	var names []string
	for _, k := range root {
		names = append(names, k.Name)
	}
	assert.ElementsMatch(t, []string{"a_b", "aXb", "a%b", "a_b2", "top.bin"}, names)

	content, ok, err := lloschema.TreeFileBytes(qg, "aXb/y.txt")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "y\n", string(content))
	_, ok, err = lloschema.TreeFileBytes(qg, "a_b")
	require.NoError(t, err)
	assert.False(t, ok, "a directory has no bytes")
}

func keys(m map[string]lloschema.TreeEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTreeFileBytes_VerifiesTheStoredBytes, on a real pinned-leyline parse:
// read_file's bytes are verified against the hash leyline stored them under,
// so an untouched file reads back (leyline's hash and mache's verification
// agree), and the same file with its bytes changed under that hash is refused,
// naming the hash, instead of served (mache-c1eb7a).
func TestTreeFileBytes_VerifiesTheStoredBytes(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "notes.txt"), []byte("hello\n"), 0o644))
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	qg := lloschema.DB(db)

	got, ok, err := lloschema.TreeFileBytes(qg, "notes.txt")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "hello\n", string(got), "leyline's blob_hash verifies under mache's BLAKE3")

	var h []byte
	require.NoError(t, db.QueryRow(`SELECT blob_hash FROM _tree WHERE path = 'notes.txt'`).Scan(&h))
	fixturedb.DamageBlob(t, db, h, fixturedb.BlobTampered)
	got, ok, err = lloschema.TreeFileBytes(qg, "notes.txt")
	require.ErrorIs(t, err, lloschema.ErrBlobTampered)
	assert.Contains(t, err.Error(), "notes.txt")
	assert.False(t, ok)
	assert.Nil(t, got)
}
