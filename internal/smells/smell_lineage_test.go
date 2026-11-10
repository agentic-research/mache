package smells

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
)

// TestLineageMismatch: a baseline's node_hash keys are comparable only with a
// scan under the same IR lineage (mache-99568c).
func TestLineageMismatch(t *testing.T) {
	v2 := smellBaseline{IRLineage: "merkle-ast-v2"}
	err := lineageMismatch(v2, "merkle-ast-v3")
	require.ErrorIs(t, err, errLineage)
	assert.Contains(t, err.Error(), "merkle-ast-v2")
	assert.Contains(t, err.Error(), "merkle-ast-v3")
	assert.Contains(t, err.Error(), "task smells:baseline")

	assert.NoError(t, lineageMismatch(v2, "merkle-ast-v2"), "same lineage")
	assert.NoError(t, lineageMismatch(smellBaseline{}, "merkle-ast-v3"), "a baseline that records none can't be checked")
	assert.NoError(t, lineageMismatch(v2, ""), "a db that records none can't be checked")
}

// TestFindSmellsCLI_RefusesABaselineFromAnotherLineage, on a real pinned-leyline
// parse: a regenerated baseline records the parse's IR lineage, and gating a
// scan against a baseline from another lineage fails with that reason (exit 4),
// never as a list of every finding turned "new" (mache-99568c).
func TestFindSmellsCLI_RefusesABaselineFromAnotherLineage(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"),
		[]byte("package a\n\nfunc Unused() int { return 1 }\n"), 0o644))
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	var dbPath string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")

	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsRule = "*"
	findSmellsDBPath = dbPath
	findSmellsFormat = "ci"
	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)

	findSmellsWriteBaseline = baselinePath
	code, err := runFindSmells(findSmellsCmd, nil)
	require.NoError(t, err)
	require.Equal(t, 0, code, buf.String())
	findSmellsWriteBaseline = ""
	base, err := loadBaseline(baselinePath)
	require.NoError(t, err)
	require.Equal(t, "merkle-ast-v3", base.IRLineage, "the baseline records the parse's lineage")

	// The same baseline, as if written under an earlier lineage.
	base.IRLineage = "merkle-ast-v0"
	raw, err := json.Marshal(base)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, raw, 0o644))

	findSmellsBaseline = baselinePath
	buf.Reset()
	code, err = runFindSmells(findSmellsCmd, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, code)
	assert.Contains(t, buf.String(), "merkle-ast-v0")
	assert.Contains(t, buf.String(), "merkle-ast-v3", "the pinned leyline's lineage")
	assert.NotContains(t, buf.String(), "NEW finding", "no diff across lineages")
}
