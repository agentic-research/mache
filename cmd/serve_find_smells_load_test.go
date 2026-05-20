package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
// reaches runSmellRule's SQL composition path.
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

// snapshotSmellRegistry captures the current registry so a test
// that mutates it via appendExternalRulesFromEnv can restore the
// original state. The package-level smellRegistry is the source
// of truth for the rules listing — tests that leave it dirty
// would corrupt subsequent test cases that expect only built-ins.
func snapshotSmellRegistry(t *testing.T) {
	t.Helper()
	saved := make([]SmellRule, len(smellRegistry))
	copy(saved, smellRegistry)
	t.Cleanup(func() { smellRegistry = saved })
}

func TestAppendExternalRulesFromEnv_EmptyEnvIsNoOp(t *testing.T) {
	snapshotSmellRegistry(t)
	added, err := appendExternalRulesFromEnv("")
	require.NoError(t, err)
	assert.Equal(t, 0, added)
}

func TestAppendExternalRulesFromEnv_AppendsLoadedRules(t *testing.T) {
	snapshotSmellRegistry(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ext1.json"), []byte(`{
		"ID": "test_appender_rule_1",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ext2.json"), []byte(`{
		"ID": "test_appender_rule_2",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	beforeCount := len(smellRegistry)
	added, err := appendExternalRulesFromEnv(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, added)
	assert.Len(t, smellRegistry, beforeCount+2)

	// Both rules are findable through the listing path.
	ids := allRuleIDs()
	assert.Contains(t, ids, "test_appender_rule_1")
	assert.Contains(t, ids, "test_appender_rule_2")
}

func TestAppendExternalRulesFromEnv_LoadErrorSkipsAllRules(t *testing.T) {
	snapshotSmellRegistry(t)
	dir := t.TempDir()
	// One valid rule, one collision with a built-in. The whole
	// load fails (fail-fast), so neither is appended — even the
	// valid one is dropped, mirroring the init() "skip the whole
	// extension on any error" contract.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_valid.json"), []byte(`{
		"ID": "test_skip_appender_valid",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b_collide.json"), []byte(`{
		"ID": "dead_code",
		"Query": "SELECT 0,0,0,0,0,0,0 FROM nodes %s"
	}`), 0o644))

	beforeCount := len(smellRegistry)
	added, err := appendExternalRulesFromEnv(dir)
	require.Error(t, err, "collision must be returned, not swallowed")
	assert.Equal(t, 0, added)
	assert.Len(t, smellRegistry, beforeCount, "registry must be untouched after a load error")
	assert.NotContains(t, allRuleIDs(), "test_skip_appender_valid",
		"the valid rule must NOT leak in when its sibling fails (atomic load)")
}

func TestAppendExternalRulesFromEnv_MissingDirIsNoOp(t *testing.T) {
	snapshotSmellRegistry(t)
	beforeCount := len(smellRegistry)
	added, err := appendExternalRulesFromEnv(filepath.Join(t.TempDir(), "no-such-dir"))
	require.NoError(t, err, "missing dir is treated as 'extension not configured'")
	assert.Equal(t, 0, added)
	assert.Len(t, smellRegistry, beforeCount)
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
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must succeed to resolve the test file path")
	exampleDir := filepath.Join(filepath.Dir(thisFile), "..", "examples", "smell-rules")

	rules, err := LoadExternalSmellRules(exampleDir)
	require.NoError(t, err, "examples/smell-rules/ must load without errors — keep the docs honest")
	require.NotEmpty(t, rules, "at least one example rule should ship")
	for _, r := range rules {
		assert.NotEmpty(t, r.ID)
		assert.NotEmpty(t, r.Query)
	}
}
