package smells

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// The CLI opens its --db raw rather than through graph.Open, and its rules
// read _ast, node_defs and node_refs by column, so it makes the same refusal
// the graph makes: a db declaring a projection mache has not been checked
// against exits 4 (the db-error class) naming the shape and the way out,
// before any rule is scored against it (mache-86e215). A db declaring the
// pinned shape runs as before.
func TestFindSmellsCLI_RefusesADBOfUnknownProjection(t *testing.T) {
	run := func(t *testing.T, version string) (code int, stdout, stderr string) {
		t.Helper()
		dbPath := writeSmellCLIFixture(t)
		db, err := sql.Open("sqlite", dbPath)
		require.NoError(t, err)
		fixturedb.DeclareProjection(t, db, version)
		require.NoError(t, db.Close())

		saved := saveCLIFlags()
		defer saved.restore()
		exit := captureExit(t)
		errOut := captureStderr(t)
		defer errOut.restore()
		findSmellsRule = "dead_code"
		findSmellsDBPath = dbPath
		findSmellsFormat = "json"

		var buf bytes.Buffer
		findSmellsCmd.SetOut(&buf)
		findSmellsCmd.SetErr(&buf)
		require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))
		return *exit, buf.String(), errOut.read()
	}

	code, stdout, stderr := run(t, "projection-v14")
	assert.Equal(t, 4, code, "an unreadable db is the db-error exit class")
	assert.Empty(t, stdout, "no findings are scored against a shape mache does not know")
	assert.Contains(t, stderr, `"projection-v14"`, "the refusal names the shape found")
	assert.Contains(t, stderr, "projection-v13", "and what this mache reads")
	assert.Contains(t, stderr, "mache leyline path", "and how to rebuild")

	code, stdout, _ = run(t, "projection-v13")
	assert.Equal(t, -1, code, "the pinned shape runs to completion")
	assert.Contains(t, stdout, "pkg/Dead", "and scores its rules")
}
