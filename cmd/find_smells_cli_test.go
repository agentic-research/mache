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

// writeLongFunctionFixture seeds an _ast-bearing fixture for rules
// like long_function that need it. Three functions: 100/60/5 lines.
func writeLongFunctionFixture(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "longfn.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT, node_kind TEXT,
			start_byte INTEGER, end_byte INTEGER,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);
		INSERT INTO _ast VALUES
		  ('big',   'main.go', 'function_declaration', 0, 100, 10,  0, 110, 0),
		  ('mid',   'main.go', 'function_declaration', 100, 200, 200, 0, 260, 0),
		  ('small', 'main.go', 'function_declaration', 200, 250, 300, 0, 305, 0);
	`)
	require.NoError(t, err)
	return dbPath
}

// TestFindSmellsCLI_DefaultMinMetricApplied pins the threshold-default
// fix for the CLI path: running `mache find-smells --rule long_function`
// without --min-metric must apply rule.DefaultMinMetric=81, mirroring
// the MCP handler. Pre-fix, the CLI's findSmellsMinMetric defaulted to
// 0 with no way to distinguish "flag not set" from "flag explicitly 0",
// so the default threshold was silently ignored on the CLI path.
func TestFindSmellsCLI_DefaultMinMetricApplied(t *testing.T) {
	dbPath := writeLongFunctionFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()

	// Drive the CLI through cobra so flag-parsing actually runs;
	// setting findSmellsMinMetric directly wouldn't trip
	// cmd.Flags().Changed("min-metric"), and the test would
	// silently exercise the wrong branch.
	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "long_function",
		"--format", "json",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))

	// big (100 lines) ≥ 81 default → fires.
	// mid (60 lines) < 81 default → filtered.
	// small (5 lines) < 81 default → filtered.
	require.Len(t, resp.Findings, 1, "default min_metric=81 must filter mid (60) and small (5)")
	assert.Equal(t, "big", resp.Findings[0].NodeID)
}

// TestFindSmellsCLI_ExplicitMinMetricZeroOverridesDefault pins the
// escape hatch: passing --min-metric 0 explicitly disables the
// rule's DefaultMinMetric and returns everything sorted by metric.
// Same semantic as the MCP handler.
func TestFindSmellsCLI_ExplicitMinMetricZeroOverridesDefault(t *testing.T) {
	dbPath := writeLongFunctionFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "long_function",
		"--format", "json",
		"--min-metric", "0",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))

	// All three functions surface; explicit 0 overrides default 81.
	require.Len(t, resp.Findings, 3, "explicit --min-metric 0 must disable DefaultMinMetric")
}

// TestFindSmellsCLI_ExplicitMinMetricLowersThreshold pins that
// values BELOW the rule's default work — a bug in the MCP handler
// fixed in #302 (SQL floor) and now mirrored on the CLI.
func TestFindSmellsCLI_ExplicitMinMetricLowersThreshold(t *testing.T) {
	dbPath := writeLongFunctionFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "long_function",
		"--format", "json",
		"--min-metric", "50",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &resp))

	// big (100) and mid (60) ≥ 50; small (5) is below.
	require.Len(t, resp.Findings, 2)
	gotIDs := []string{resp.Findings[0].NodeID, resp.Findings[1].NodeID}
	assert.ElementsMatch(t, []string{"big", "mid"}, gotIDs)
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
