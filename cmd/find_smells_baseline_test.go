package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindSmellsCLI_BaselineRatchet_EndToEnd is the W5.2 "it actually works"
// proof (mache-4d155c): a REAL find-smells scan over a fixture .db, gated by
// the count-ratchet. Writing a baseline grandfathers the current finding;
// re-running within it passes (exit 0); running against an empty baseline
// makes that same finding new debt and fails (exit 1).
func TestFindSmellsCLI_BaselineRatchet_EndToEnd(t *testing.T) {
	dbPath := writeSmellCLIFixture(t) // one dead_code finding: pkg/Dead
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")

	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsRule = "dead_code"
	findSmellsDBPath = dbPath
	findSmellsFormat = "ci"

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)

	// 1. Regenerate the baseline from the current scan → exit 0, file written.
	findSmellsWriteBaseline = baselinePath
	code, err := runFindSmells(findSmellsCmd, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, code, "--write-baseline exits 0")
	require.FileExists(t, baselinePath)
	findSmellsWriteBaseline = ""

	// 2. Gate against that baseline: current == baseline → no new debt → exit 0.
	findSmellsBaseline = baselinePath
	code, err = runFindSmells(findSmellsCmd, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, code, "findings within the baseline → gate passes")

	// 3. Gate against an EMPTY baseline: the dead_code finding is now new debt
	//    → exit 1, and the new finding is reported. This is the gate biting.
	emptyBaseline := filepath.Join(t.TempDir(), "empty.json")
	require.NoError(t, os.WriteFile(emptyBaseline, []byte(`{"version":1,"counts":[]}`), 0o644))
	findSmellsBaseline = emptyBaseline
	buf.Reset()
	code, err = runFindSmells(findSmellsCmd, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, code, "a finding absent from the baseline is new debt → gate fails")
	assert.Contains(t, buf.String(), "NEW finding")
	assert.Contains(t, buf.String(), "dead_code")
}

// TestBaseline_LoadWriteRoundTrip pins the on-disk baseline format.
func TestBaseline_LoadWriteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	orig := computeBaseline([]smellFinding{
		rf("long_function", "a.go", 10),
		rf("long_function", "a.go", 40),
		rf("dead_code", "b.go", 3),
	})
	require.NoError(t, writeBaseline(path, orig))

	got, err := loadBaseline(path)
	require.NoError(t, err)
	assert.Equal(t, 2, got.lookup("long_function", "a.go"))
	assert.Equal(t, 1, got.lookup("dead_code", "b.go"))
}
