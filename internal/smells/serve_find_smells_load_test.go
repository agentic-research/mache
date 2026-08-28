package smells

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadExternalSmellRules_HappyPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.json"), []byte(`{
		"ID": "custom_helper_count",
		"Description": "External rule loaded from JSON",
		"Requires": ["nodes"],
		"ScopeColumn": "n.source_file",
		"Query": "SELECT COALESCE(n.source_file, '') AS source_id, n.id, 0, 0, 0, 0, COUNT(*) FROM nodes n GROUP BY n.source_file %s ORDER BY 1"
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "custom_helper_count", rules[0].ID)
	assert.Equal(t, []string{"nodes"}, rules[0].Requires)
	assert.Contains(t, rules[0].Query, "%s")
}

func TestLoadExternalSmellRules_EmptyDirReturnsNil(t *testing.T) {
	rules, err := LoadExternalSmellRules(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestLoadExternalSmellRules_MissingDirIsTolerated(t *testing.T) {
	// Non-existent dir: not an error — same as "no external rules
	// configured." Otherwise every dev environment without the dir
	// would fail to start.
	rules, err := LoadExternalSmellRules(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestLoadExternalSmellRules_EmptyDirArgReturnsNil(t *testing.T) {
	// Production callers pass os.Getenv() result directly, which is ""
	// when the env var isn't set. Treat empty as "feature disabled."
	rules, err := LoadExternalSmellRules("")
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestLoadExternalSmellRules_RejectsCollisionWithBuiltin(t *testing.T) {
	dir := t.TempDir()
	// 'dead_code' is a built-in — must not be silently shadowed.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "shadow.json"), []byte(`{
		"ID": "dead_code",
		"Description": "Tries to shadow a built-in",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err, "duplicate ID with a built-in must be rejected")
	assert.Contains(t, err.Error(), "already defined")
	assert.Contains(t, err.Error(), "built-in")
}

func TestLoadExternalSmellRules_RejectsCollisionBetweenExternals(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{
		"ID": "twin",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{
		"ID": "twin",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already defined")
}

func TestLoadExternalSmellRules_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"),
		[]byte(`{not valid json`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLoadExternalSmellRules_RejectsMissingID(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "no_id.json"), []byte(`{
		"Description": "no id",
		"Query": "SELECT 1 %s"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ID is required")
}

func TestLoadExternalSmellRules_RejectsMissingPlaceholder(t *testing.T) {
	dir := t.TempDir()
	// Query without %s placeholder — would silently fail to splice
	// the scope clause. Reject at load time.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "no_placeholder.json"), []byte(`{
		"ID": "no_placeholder",
		"Query": "SELECT 0, 0, 0, 0, 0, 0, 0 FROM nodes"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "%s")
}

// TestLoadExternalSmellRules_HonorsDefaultMinMetric pins that
// external JSON rules can set DefaultMinMetric and the value
// round-trips through the loader. Important for rule authors:
// without this guarantee, a rule that depends on a default
// threshold (the way long_function does — see #302) couldn't
// be authored externally and would have to ship in-tree.
func TestLoadExternalSmellRules_HonorsDefaultMinMetric(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "with_default.json"), []byte(`{
		"ID": "test_with_default",
		"Description": "Rule with a default min_metric",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s",
		"DefaultMinMetric": 42
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, int64(42), rules[0].DefaultMinMetric,
		"DefaultMinMetric must round-trip from JSON to SmellRule")
}

func TestLoadExternalSmellRules_DefaultMinMetricOmittedDefaultsZero(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "no_default.json"), []byte(`{
		"ID": "test_no_default",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, int64(0), rules[0].DefaultMinMetric,
		"omitted DefaultMinMetric must zero-value (no default → caller's min_metric is sole control)")
}

func TestLoadExternalSmellRules_RejectsUnescapedPercent(t *testing.T) {
	dir := t.TempDir()
	// SQL LIKE with unescaped '%' — would be interpreted as fmt
	// verb at runtime and corrupt the query (yielding %!f(string=...)
	// or similar). Must be rejected at load time. Authors should
	// escape as '%%' (e.g. LIKE '%%foo%%').
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad_like.json"), []byte(`{
		"ID": "bad_like_pattern",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes WHERE name LIKE '%foo%' %s"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unescaped")
	assert.Contains(t, err.Error(), "%%")
}

func TestLoadExternalSmellRules_AcceptsEscapedPercent(t *testing.T) {
	dir := t.TempDir()
	// Properly-escaped LIKE patterns must pass validation. This
	// matches how the built-in rules write their LIKE clauses.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good_like.json"), []byte(`{
		"ID": "good_like_pattern",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes WHERE name LIKE '%%foo%%' %s"
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "good_like_pattern", rules[0].ID)
}

func TestLoadExternalSmellRules_IgnoresNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# notes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.yaml"), []byte("ignored: true"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.json"), []byte(`{
		"ID": "real_rule",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "real_rule", rules[0].ID)
}

func TestLoadExternalSmellRules_FilePassedAsDirRejected(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir.json")
	require.NoError(t, os.WriteFile(f, []byte(`{}`), 0o644))

	_, err := LoadExternalSmellRules(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// TestValidateScopeColumn_AcceptsBuiltinShapes pins that every
// ScopeColumn value used by built-in rules passes the load-time
// validator. If a future contributor tightens the whitelist in a way
// that breaks a real shape, this catches it before the loader starts
// rejecting legitimate external rules that copy the built-in pattern.
func TestValidateScopeColumn_AcceptsBuiltinShapes(t *testing.T) {
	for _, r := range smellRegistry {
		if r.ScopeColumn == "" {
			continue
		}
		t.Run(r.ID, func(t *testing.T) {
			require.NoError(t, validateScopeColumn(r.ScopeColumn),
				"built-in ScopeColumn %q must pass validation", r.ScopeColumn)
		})
	}
}

// TestValidateScopeColumn_RejectsInjectionShapes asserts the
// security hardening: even though the trust boundary for external
// rules is "operator controls the rules dir", a malicious or buggy
// rule file cannot smuggle a `;`-terminated second statement, a
// `--` line comment, or any character outside the small whitelist
// that the built-in registry actually uses.
func TestValidateScopeColumn_RejectsInjectionShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"semicolon terminator", "n.source_file; DROP TABLE nodes", "statement terminator"},
		{"line comment", "n.source_file --", "line comment"},
		{"slash-star block comment", "n.source_file /* foo */", "disallowed character"},
		{"asterisk wildcard", "n.*", "disallowed character"},
		{"equals comparator", "n.source_file = 'x'", "disallowed character"},
		{"backtick", "`n.source_file`", "disallowed character"},
		{"double quote identifier", `"n.source_file"`, "disallowed character"},
		{"newline injection", "n.source_file\nDROP TABLE", "disallowed character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateScopeColumn(c.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// TestLoadExternalSmellRules_RejectsInjectableScopeColumn is the
// end-to-end version: a JSON file whose ScopeColumn carries a `;`
// must not load. Closes the loop on the user's "ensure we are
// making our queries safe / not making them injectable" directive
// for the only surface where external (non-source-tree) input
// reaches RunSmellRule's SQL composition path.
func TestLoadExternalSmellRules_RejectsInjectableScopeColumn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "evil.json"), []byte(`{
		"ID": "evil_scope",
		"Description": "Tries to escape the scope clause",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s",
		"ScopeColumn": "n.source_file; DROP TABLE nodes; --"
	}`), 0o644))

	_, err := LoadExternalSmellRules(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ScopeColumn")
	assert.Contains(t, err.Error(), "statement terminator")
}

// TestLoadExternalSmellRules_AcceptsBuiltinShapedScopeColumn pins
// that a rule using the same ScopeColumn shapes the built-ins use
// (dotted identifiers, COALESCE/NULLIF) loads cleanly. Without
// this, the hardening could quietly grow stricter than the
// built-ins and lock out reasonable external rules.
func TestLoadExternalSmellRules_AcceptsBuiltinShapedScopeColumn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coalesce.json"), []byte(`{
		"ID": "external_coalesce_scope",
		"Description": "Uses the same COALESCE shape several built-ins use",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes n %s",
		"ScopeColumn": "COALESCE(NULLIF(n.source_file, ''), '')"
	}`), 0o644))

	rules, err := LoadExternalSmellRules(dir)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "external_coalesce_scope", rules[0].ID)
}

// activeSmellRules replaces the old init()-time append path: externals
// are merged with a fresh copy of the built-in registry PER CALL, never
// mutating the global. The tests below migrate the coverage that used to
// live on appendExternalRulesFromEnv onto activeSmellRules.

func TestActiveSmellRules_EmptyDirReturnsBuiltinsOnly(t *testing.T) {
	rules, err := activeSmellRules("")
	require.NoError(t, err)
	assert.Len(t, rules, len(smellRegistry), "empty dir must yield exactly the built-ins")
}

func TestActiveSmellRules_DoesNotMutateGlobalRegistry(t *testing.T) {
	before := len(smellRegistry)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ext.json"), []byte(`{
		"ID": "test_active_no_mutate",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	rules, err := activeSmellRules(dir)
	require.NoError(t, err)
	assert.Len(t, rules, before+1, "returned set must include the external rule")
	assert.Len(t, smellRegistry, before,
		"activeSmellRules must NOT append to the package global (no init-time mutation)")
}

func TestActiveSmellRules_MergesBuiltinsAndExternal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ext1.json"), []byte(`{
		"ID": "test_active_rule_1",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ext2.json"), []byte(`{
		"ID": "test_active_rule_2",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	rules, err := activeSmellRules(dir)
	require.NoError(t, err)

	ids := allRuleIDs(rules)
	assert.Contains(t, ids, "test_active_rule_1")
	assert.Contains(t, ids, "test_active_rule_2")
	assert.Contains(t, ids, "dead_code", "built-ins must still be present alongside externals")
}

func TestActiveSmellRules_RejectsCollisionButKeepsBuiltins(t *testing.T) {
	dir := t.TempDir()
	// A valid rule plus one colliding with a built-in. The load is
	// fail-fast so the whole external set is dropped, but the built-ins
	// come back with the error so serve can fall back to them — the
	// safety property the old init() skip-on-error contract guaranteed.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_valid.json"), []byte(`{
		"ID": "test_active_valid",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b_collide.json"), []byte(`{
		"ID": "dead_code",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	rules, err := activeSmellRules(dir)
	require.Error(t, err, "collision must be surfaced, not swallowed")
	assert.Contains(t, err.Error(), "already defined")
	assert.Len(t, rules, len(smellRegistry),
		"on load error the built-ins are returned so serve can fall back")
	assert.NotContains(t, allRuleIDs(rules), "test_active_valid",
		"the valid rule must NOT leak in when its sibling fails (atomic load)")
}

func TestActiveSmellRules_MissingDirReturnsBuiltins(t *testing.T) {
	rules, err := activeSmellRules(filepath.Join(t.TempDir(), "no-such-dir"))
	require.NoError(t, err, "missing dir is treated as 'not configured'")
	assert.Len(t, rules, len(smellRegistry))
}

// TestActiveSmellRules_LiveReload is the KEY behavior this feature ships:
// a rule JSON added to the dir AFTER a first call is visible on the
// SECOND call, with no re-init, no restart, no reconnect. This is the
// live-reload property — the whole point of moving the load off init().
func TestActiveSmellRules_LiveReload(t *testing.T) {
	dir := t.TempDir()

	// First call: empty dir -> built-ins only.
	first, err := activeSmellRules(dir)
	require.NoError(t, err)
	assert.NotContains(t, allRuleIDs(first), "test_live_reload_rule",
		"rule must not exist before its file is dropped")
	firstCount := len(first)

	// Drop a new rule file into the same dir — simulating an operator
	// adding a rule while the daemon is up.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "late.json"), []byte(`{
		"ID": "test_live_reload_rule",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	// Second call rescans the dir — no re-init in between — and sees it.
	second, err := activeSmellRules(dir)
	require.NoError(t, err)
	assert.Contains(t, allRuleIDs(second), "test_live_reload_rule",
		"a rule added after the first call must appear on the second (live reload)")
	assert.Len(t, second, firstCount+1)
}

// TestLoadExternalSmellRules_ShippedExamplesLoadCleanly is the
// "don't ship a broken example" regression. Anything we put in
// examples/smell-rules/ has to round-trip through the loader
// without errors — otherwise the docs lie. If a future contributor
// adds an example that doesn't validate, this test catches it
// before merge.
//
// Resolves the examples dir via runtime.Caller so the path is
// anchored to the test source file rather than the package's
// working directory. macOS CI runners on `go test ./...` produced
// flaky NotExist errors with a relative `../examples/smell-rules`
// path; runtime.Caller is invariant across platforms and
// invocation styles.
func TestLoadExternalSmellRules_ShippedExamplesLoadCleanly(t *testing.T) {
	// testutil.MacheRepoRoot, not a hand-counted runtime.Caller hop: the
	// hand-counted ".." broke silently when this file moved from cmd/ to
	// internal/smells/ (mache-96c378 stage 4).
	exampleDir := filepath.Join(testutil.MacheRepoRoot(t), "examples", "smell-rules")

	rules, err := LoadExternalSmellRules(exampleDir)
	require.NoError(t, err, "examples/smell-rules/ must load without errors — keep the docs honest")
	require.NotEmpty(t, rules, "at least one example rule should ship")
	for _, r := range rules {
		assert.NotEmpty(t, r.ID)
		assert.NotEmpty(t, r.Query)
	}
}
