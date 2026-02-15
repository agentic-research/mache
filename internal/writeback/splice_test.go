package writeback

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "splice-test-*")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestSplice_ReplaceMiddle(t *testing.T) {
	path := tempFile(t, "func A() {}\nfunc B() {}\nfunc C() {}\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 12, // start of "func B() {}\n"
		EndByte:   24, // end of "func B() {}\n"
	}
	err := Splice(origin, []byte("func B() { return 1 }\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func A() {}\nfunc B() { return 1 }\nfunc C() {}\n", string(got))
}

func TestSplice_ShorterContent(t *testing.T) {
	path := tempFile(t, "func LongName() { /* lots of code */ }\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   39,
	}
	err := Splice(origin, []byte("func S() {}\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func S() {}\n", string(got))
}

func TestSplice_LongerContent(t *testing.T) {
	path := tempFile(t, "func S() {}\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   12,
	}
	err := Splice(origin, []byte("func LongName() { /* lots of code */ }\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func LongName() { /* lots of code */ }\n", string(got))
}

func TestSplice_EmptyContent(t *testing.T) {
	path := tempFile(t, "AAA\nBBB\nCCC\n")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 4, // "BBB\n"
		EndByte:   8,
	}
	err := Splice(origin, []byte{})
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "AAA\nCCC\n", string(got))
}

func TestSplice_InvalidRange(t *testing.T) {
	path := tempFile(t, "short")
	// EndByte beyond file length
	err := Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   100,
	}, []byte("x"))
	assert.Error(t, err)

	// StartByte > EndByte
	err = Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 3,
		EndByte:   1,
	}, []byte("x"))
	assert.Error(t, err)
}

func TestSplice_PreservesPermissions(t *testing.T) {
	path := tempFile(t, "content")
	require.NoError(t, os.Chmod(path, 0o755))

	err := Splice(graph.SourceOrigin{
		FilePath:  path,
		StartByte: 0,
		EndByte:   7,
	}, []byte("new"))
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestSplice_NonexistentFile(t *testing.T) {
	err := Splice(graph.SourceOrigin{
		FilePath:  filepath.Join(t.TempDir(), "nope.go"),
		StartByte: 0,
		EndByte:   5,
	}, []byte("x"))
	assert.Error(t, err)
}
