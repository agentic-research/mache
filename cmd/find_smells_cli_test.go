package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

// captureExit installs a no-op exitFunc that records the exit code into
// the returned int pointer instead of terminating the test binary. The
// snapshot.restore() registered earlier will put the real os.Exit back
// when the test finishes. Tests check `*code` to assert exit semantics.
func captureExit(t *testing.T) *int {
	t.Helper()
	code := -1
	exitFunc = func(c int) { code = c }
	return &code
}

// registerTestRule appends a SmellRule to smellRegistry and registers a
// t.Cleanup to slice it back off. The rule's SQL just selects rows from
// the test fixture so we get deterministic findings without standing
// up a full _ast graph. The pre-flight check requires only `nodes`,
// which writeSmellCLIFixture creates.
func registerTestRule(t *testing.T, rule SmellRule) {
	t.Helper()
	smellRegistry = append(smellRegistry, rule)
	t.Cleanup(func() {
		// Strip the trailing entry we just added. Other tests that mutate
		// the registry use snapshotSmellRegistry — these new tests
		// surgically pop the last N entries so they compose with that
		// helper without ordering hazards.
		for i := len(smellRegistry) - 1; i >= 0; i-- {
			if smellRegistry[i].ID == rule.ID {
				smellRegistry = append(smellRegistry[:i], smellRegistry[i+1:]...)
				return
			}
		}
	})
}

// testRuleQuery emits one row per node so a fixture's three seeded
// nodes produce three findings. The `%s` is the scope clause splice
// every rule requires; the leading "AND 1=1 %s" keeps the splice valid
// whether scope is empty or `AND ... = ?`.
const testRuleQuery = `
	SELECT n.id AS source_id,
	       n.id AS node_id,
	       0 AS start_byte, 0 AS end_byte,
	       0 AS start_row, 0 AS start_col,
	       0 AS metric
	FROM nodes n
	WHERE 1=1 %s
	ORDER BY n.id
`

// TestFindSmellsCLI_FailOn_None_NeverFails pins the observability-contract
// escape hatch: `--fail-on=none` MUST exit 0 even when findings exist.
// This is the only setting that preserves the pre-ADR-0018 documented
// behavior, so any change to the default-semantic path that breaks this
// is a contract regression.
func TestFindSmellsCLI_FailOn_None_NeverFails(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()
	code := captureExit(t)

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "dead_code",
		"--format", "json",
		"--fail-on", "none",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	assert.Equal(t, -1, *code, "exitFunc must not be called when --fail-on=none")
	assert.Contains(t, buf.String(), "pkg/Dead",
		"finding output must still be emitted; --fail-on=none mutes the gate, not the rule")
}

// TestFindSmellsCLI_FailOn_Warn_FailsOnWarnRule asserts that
// --fail-on=warn escalates a default-severity (warn) rule's findings
// into a non-zero exit. This is the clippy `-D warnings` analogue —
// CI opt-in to treat warnings as fatal.
func TestFindSmellsCLI_FailOn_Warn_FailsOnWarnRule(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()
	code := captureExit(t)

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "dead_code", // default Severity ("") => warn
		"--format", "json",
		"--fail-on", "warn",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	assert.Equal(t, 1, *code,
		"--fail-on=warn must exit 1 when a warn-severity rule emits findings")
}

// TestFindSmellsCLI_FailOn_Error_OnlyFailsOnErrorRule pins the default
// --fail-on=error semantics: warn-severity rules pass even with
// findings, but a rule explicitly marked Severity=error fails the gate.
// This is the contract that lets the default be `error` without
// silently breaking the observability promise — every shipped rule
// today is warn-severity, so the default behavior is "no gate" until
// a rule author opts in.
func TestFindSmellsCLI_FailOn_Error_OnlyFailsOnErrorRule(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)

	// Warn rule: default --fail-on=error must NOT escalate.
	t.Run("warn rule passes the default error gate", func(t *testing.T) {
		saved := saveCLIFlags()
		defer saved.restore()
		code := captureExit(t)

		var buf bytes.Buffer
		findSmellsCmd.SetOut(&buf)
		findSmellsCmd.SetErr(&buf)
		require.NoError(t, findSmellsCmd.ParseFlags([]string{
			"--db", dbPath,
			"--rule", "dead_code",
			"--format", "json",
			"--fail-on", "error",
		}))
		require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

		assert.Equal(t, -1, *code,
			"warn-severity rules must NOT trigger --fail-on=error (default observability contract)")
	})

	// Error rule: same default --fail-on=error MUST escalate.
	t.Run("error-severity rule trips the default error gate", func(t *testing.T) {
		saved := saveCLIFlags()
		defer saved.restore()
		code := captureExit(t)

		registerTestRule(t, SmellRule{
			ID:          "test_error_rule",
			Description: "Test-only error-severity rule for --fail-on=error coverage.",
			Severity:    SeverityError,
			Requires:    []string{"nodes"},
			Query:       testRuleQuery,
		})

		var buf bytes.Buffer
		findSmellsCmd.SetOut(&buf)
		findSmellsCmd.SetErr(&buf)
		require.NoError(t, findSmellsCmd.ParseFlags([]string{
			"--db", dbPath,
			"--rule", "test_error_rule",
			"--format", "json",
			"--fail-on", "error",
		}))
		require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

		assert.Equal(t, 1, *code,
			"--fail-on=error MUST exit 1 when an error-severity rule emits findings")
	})
}

// TestFindSmellsCLI_Rule_GlobMatchesMultiple pins the glob extension
// (ADR-0018): `--rule='test_*'` runs every rule whose ID matches,
// not just the first registry hit. The output for both rules must
// appear so a downstream consumer (CI script, GHA) sees both rule IDs.
func TestFindSmellsCLI_Rule_GlobMatchesMultiple(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()
	captureExit(t) // findings would not gate; warn rules + default fail-on=error

	registerTestRule(t, SmellRule{
		ID:          "test_a",
		Description: "Test rule A for glob coverage.",
		Requires:    []string{"nodes"},
		Query:       testRuleQuery,
	})
	registerTestRule(t, SmellRule{
		ID:          "test_b",
		Description: "Test rule B for glob coverage.",
		Requires:    []string{"nodes"},
		Query:       testRuleQuery,
	})

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "test_*",
		"--format", "json",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	out := buf.String()
	// JSON renderer emits one envelope per rule; both rule IDs must surface.
	assert.Contains(t, out, `"rule": "test_a"`,
		"glob test_* must match test_a")
	assert.Contains(t, out, `"rule": "test_b"`,
		"glob test_* must also match test_b (not just the first hit)")
}

// TestFindSmellsCLI_Rule_GlobMatchesNone_Exit3 pins the
// no-match error path: when the glob (and tags) filter produces zero
// rules, exit 3 fires and the message includes the glob so the user
// can debug their pattern.
func TestFindSmellsCLI_Rule_GlobMatchesNone_Exit3(t *testing.T) {
	saved := saveCLIFlags()
	defer saved.restore()
	code := captureExit(t)

	// stderr capture — the printAndCode error goes to os.Stderr, not
	// cmd.ErrOrStderr, so we redirect the package var. (Cobra's
	// SetErr doesn't intercept os.Stderr writes.)
	stderr := captureStderr(t)
	defer stderr.restore()

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", "/dev/null", // never reached; matcher fails first
		"--rule", "no_such_*",
		"--format", "json",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	assert.Equal(t, 3, *code, "unmatched --rule glob must exit 3")
	got := stderr.read()
	assert.Contains(t, got, "no_such_*",
		"exit 3 message must name the glob so the failure is debuggable")
}

// TestFindSmellsCLI_Tags_FiltersToMatchingRules pins that --tags
// filters the registry via set-union semantics on rule.Tags. Two test
// rules with disjoint tags; --tags=docs picks only the docs-tagged one.
func TestFindSmellsCLI_Tags_FiltersToMatchingRules(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()
	captureExit(t)

	registerTestRule(t, SmellRule{
		ID:          "test_foo",
		Description: "Test rule tagged docs.",
		Tags:        []string{"docs"},
		Requires:    []string{"nodes"},
		Query:       testRuleQuery,
	})
	registerTestRule(t, SmellRule{
		ID:          "test_bar",
		Description: "Test rule tagged security.",
		Tags:        []string{"security"},
		Requires:    []string{"nodes"},
		Query:       testRuleQuery,
	})

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "test_*",
		"--tags", "docs",
		"--format", "json",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	out := buf.String()
	assert.Contains(t, out, `"rule": "test_foo"`,
		"--tags=docs must run the docs-tagged rule")
	assert.NotContains(t, out, `"rule": "test_bar"`,
		"--tags=docs must NOT run the security-tagged rule (union, not catch-all)")
}

// TestFindSmellsCLI_Format_CI_OutputShape pins the `--format=ci` line
// shape: `<source_id>:<line>:<column>: <severity>: <message> [<rule-id>]`.
// One assertion per emitted finding so the test catches both shape
// drift AND missing-finding regressions.
func TestFindSmellsCLI_Format_CI_OutputShape(t *testing.T) {
	dbPath := writeSmellCLIFixture(t)
	saved := saveCLIFlags()
	defer saved.restore()
	captureExit(t)

	var buf bytes.Buffer
	findSmellsCmd.SetOut(&buf)
	findSmellsCmd.SetErr(&buf)
	require.NoError(t, findSmellsCmd.ParseFlags([]string{
		"--db", dbPath,
		"--rule", "dead_code",
		"--format", "ci",
	}))
	require.NoError(t, findSmellsCmd.RunE(findSmellsCmd, nil))

	out := strings.TrimSpace(buf.String())
	require.NotEmpty(t, out, "CI format must produce at least one finding line")

	// Pattern: file:line:col: severity: message [rule-id]
	// - file: greedy non-colon-or-space match (dead_code fixture uses
	//   plain filenames like "dead.go")
	// - severity: one of off/warn/error (warn for dead_code today)
	// - message: anything non-greedy up to the trailing [rule-id]
	pat := regexp.MustCompile(`^[^\s:]+:\d+:\d+: (off|warn|error): .+ \[[a-z_][a-z0-9_]*\]$`)
	for i, line := range strings.Split(out, "\n") {
		assert.Regexp(t, pat, line,
			"CI line %d does not match `<source_id>:<line>:<col>: <severity>: <msg> [<rule-id>]`: %q", i, line)
	}
}

// captureStderr swaps os.Stderr for a pipe so tests can read what
// printAndCode wrote (the unknown-rule error message). Restored via
// the returned struct's .restore(). Kept here (not in the snapshot)
// because not every test needs it.
type stderrCapture struct {
	orig *os.File
	r, w *os.File
}

func captureStderr(t *testing.T) *stderrCapture {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	return &stderrCapture{orig: orig, r: r, w: w}
}

func (s *stderrCapture) read() string {
	_ = s.w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(s.r)
	return buf.String()
}

func (s *stderrCapture) restore() {
	// Close pipe ends in case read() wasn't called (e.g. test aborted
	// before assertion). Best-effort — both writes after Close are
	// no-ops on a closed *os.File.
	_ = s.w.Close()
	_ = s.r.Close()
	os.Stderr = s.orig
}

// cliFlagSnapshot lets tests mutate the package-level flag vars without
// leaking state across tests (cobra binds them globally). Extended in
// ADR-0018 PR 2 to cover --fail-on / --tags and the exitFunc override.
type cliFlagSnapshot struct {
	rule, db, sourceID, format, failOn, tags string
	baseline, writeBaseline                  string
	limit                                    int
	minMetric                                int64
	exitFunc                                 func(int)
}

func saveCLIFlags() cliFlagSnapshot {
	return cliFlagSnapshot{
		rule:          findSmellsRule,
		db:            findSmellsDBPath,
		sourceID:      findSmellsSourceID,
		format:        findSmellsFormat,
		failOn:        findSmellsFailOn,
		tags:          findSmellsTags,
		baseline:      findSmellsBaseline,
		writeBaseline: findSmellsWriteBaseline,
		limit:         findSmellsLimit,
		minMetric:     findSmellsMinMetric,
		exitFunc:      exitFunc,
	}
}

func (s cliFlagSnapshot) restore() {
	findSmellsRule = s.rule
	findSmellsDBPath = s.db
	findSmellsSourceID = s.sourceID
	findSmellsFormat = s.format
	findSmellsFailOn = s.failOn
	findSmellsTags = s.tags
	findSmellsBaseline = s.baseline
	findSmellsWriteBaseline = s.writeBaseline
	findSmellsLimit = s.limit
	findSmellsMinMetric = s.minMetric
	exitFunc = s.exitFunc
}
