package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// writeSmellCLIFixture seeds a minimal nodes/node_defs/node_refs db
// with one live + one dead symbol so dead_code returns the dead one.
// Mirrors TestFindSmells_DeadCode but writes to a real file path so
// the CLI can sql.Open it.
func writeSmellCLIFixture(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "smells.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER, mtime INTEGER, source_file TEXT, record TEXT);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));

		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/Live',   'pkg', 'Live',   1, 0, 'live.go',   ''),
		  ('pkg/Caller', 'pkg', 'Caller', 1, 0, 'caller.go', ''),
		  ('pkg/Dead',   'pkg', 'Dead',   1, 0, 'dead.go',   '');

		INSERT INTO node_defs VALUES ('Live', 'pkg/Live'), ('Dead', 'pkg/Dead');
		INSERT INTO node_refs VALUES ('Live', 'pkg/Caller');
	`)
	require.NoError(t, err)
	return dbPath
}

// TestFindSmellsCLI_ListRulesNoDB asserts discovery mode works without
// a --db flag and that requires/languages survive the JSON round-trip.
func TestFindSmellsCLI_ListRulesNoDB(t *testing.T) {
	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsFormat = "json"

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var resp struct {
		Rules []struct {
			ID       string   `json:"id"`
			Requires []string `json:"requires"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))

	ids := make(map[string][]string, len(resp.Rules))
	for _, r := range resp.Rules {
		ids[r.ID] = r.Requires
	}
	assert.Contains(t, ids, "dead_code")
	assert.Contains(t, ids["dead_code"], "node_refs")
}

// TestFindSmellsCLI_RunRuleAgainstFixture exercises the full CLI path:
// open db, pre-flight, run rule, render JSON.
func TestFindSmellsCLI_RunRuleAgainstFixture(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)

	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsRule = "dead_code"
	findSmellsDBPath = dbPath
	findSmellsFormat = "json"

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var resp struct {
		Rule     string         `json:"rule"`
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))
	assert.Equal(t, "dead_code", resp.Rule)
	require.Equal(t, 1, resp.Total, "only Dead has no ref")
	assert.Equal(t, "pkg/Dead", resp.Findings[0].NodeID)
}

// TestFindSmellsCLI_MarkdownRender checks the human-readable format
// renders a header and a table row for the finding.
func TestFindSmellsCLI_MarkdownRender(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)

	saved := saveCLIFlags()
	defer saved.restore()
	findSmellsRule = "dead_code"
	findSmellsDBPath = dbPath
	findSmellsFormat = "md"

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	out := buf.String()
	assert.True(t, strings.HasPrefix(out, "# find_smells: `dead_code`"), "missing markdown header: %q", out[:80])
	assert.Contains(t, out, "**1 findings**")
	assert.Contains(t, out, "| pkg/Dead |")
}

// cliFlagSnapshot lets tests mutate the package-level flag vars without
// leaking state across tests (cobra binds them globally).
type cliFlagSnapshot struct {
	rule, db, sourceID, format string
	limit                      int
	minMetric                  int64
}

func saveCLIFlags() cliFlagSnapshot {
	return cliFlagSnapshot{
		rule:      findSmellsRule,
		db:        findSmellsDBPath,
		sourceID:  findSmellsSourceID,
		format:    findSmellsFormat,
		limit:     findSmellsLimit,
		minMetric: findSmellsMinMetric,
	}
}

func (s cliFlagSnapshot) restore() {
	findSmellsRule = s.rule
	findSmellsDBPath = s.db
	findSmellsSourceID = s.sourceID
	findSmellsFormat = s.format
	findSmellsLimit = s.limit
	findSmellsMinMetric = s.minMetric
}
