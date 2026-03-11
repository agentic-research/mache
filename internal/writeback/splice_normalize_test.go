package writeback

import (
	"os"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #6: Write-back adds trailing newline artifact
// When an agent writes via echo/heredoc, the content gets a trailing \n
// that wasn't in the original. Splice should normalize trailing whitespace
// to match the original region.

func TestSplice_NormalizesTrailingNewline(t *testing.T) {
	// Original file: constant declaration without trailing newline at the end of the region
	original := "package main\n\nconst maxRetries = 3\n\nfunc main() {}\n"
	path := tempFile(t, original)

	// The constant region is "const maxRetries = 3" (no trailing \n in the capture)
	// StartByte=14 (after "package main\n\n"), EndByte=34 ("const maxRetries = 3")
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 14,
		EndByte:   34,
	}

	// Agent writes with trailing newline (from echo command)
	newContent := []byte("const maxRetries = 5\n")

	err := Splice(origin, newContent)
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	// The result should NOT have an extra blank line
	assert.Equal(t, "package main\n\nconst maxRetries = 5\n\nfunc main() {}\n", string(got),
		"splice should normalize trailing newline to match original region")
}

func TestSplice_PreservesTrailingNewlineWhenOriginalHasOne(t *testing.T) {
	// Original region ends with \n
	original := "func A() {}\nfunc B() {}\nfunc C() {}\n"
	path := tempFile(t, original)

	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 12,
		EndByte:   24, // "func B() {}\n" — region includes the \n
	}

	// Agent writes with trailing newline — should be preserved since original had one
	err := Splice(origin, []byte("func B() { return 1 }\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "func A() {}\nfunc B() { return 1 }\nfunc C() {}\n", string(got))
}

func TestSplice_NoNewlineOriginal_AgentAddsNewline(t *testing.T) {
	// Original: region does NOT end with \n
	original := "x = 10\ny = 20\nz = 30\n"
	path := tempFile(t, original)

	// Region "y = 20" at bytes [7:13] — no trailing \n
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 7,
		EndByte:   13,
	}

	// Agent writes with trailing \n (echo artifact)
	err := Splice(origin, []byte("y = 25\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	// Should strip the agent's \n since original region didn't have one
	assert.Equal(t, "x = 10\ny = 25\nz = 30\n", string(got),
		"should strip trailing newline to match original region pattern")
}

func TestSplice_MultipleTrailingNewlines(t *testing.T) {
	// Agent accidentally writes multiple trailing newlines
	original := "line1\nline2\nline3\n"
	path := tempFile(t, original)

	// Region "line2" at bytes [6:11] — no trailing \n
	origin := graph.SourceOrigin{
		FilePath:  path,
		StartByte: 6,
		EndByte:   11,
	}

	// Agent writes with two trailing newlines
	err := Splice(origin, []byte("LINE2\n\n"))
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	assert.Equal(t, "line1\nLINE2\nline3\n", string(got),
		"should strip all extra trailing newlines to match original region")
}
