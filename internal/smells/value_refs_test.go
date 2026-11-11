package smells

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testutil"
)

// TestValueRef_GoAndSQLAgree pins the value-ref predicate in both encodings.
func TestValueRef_GoAndSQLAgree(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, tc := range []struct {
		kind, lang string
		want       bool
	}{
		{"argument_list", "go", true},
		{"var_spec", "go", true},
		{"return_statement", "go", true},
		{"arguments", "rust", true},
		{"let_declaration", "rust", true},
		{"block", "rust", true},
		{"keyword_argument", "python", true},
		{"method_reference", "java", true},
		{"call_expression", "go", false},
		{"macro_invocation", "rust", false},
		{"call", "python", false},
		{"method_invocation", "java", false},
		{"object_creation_expression", "java", false},
		{"command", "bash", false},
		{"block", "hcl", false},
		{"relation", "sql", false},
		{"", "markdown-inline", false},
		{"", "go", false},
	} {
		assert.Equal(t, tc.want, lloschema.IsValueRef(tc.kind, tc.lang), "Go: %s/%s", tc.lang, tc.kind)
		var kind any = tc.kind
		if tc.kind == "" {
			kind = nil // a row with no kind stores NULL
		}
		var got bool
		require.NoError(t, db.QueryRow(`SELECT `+lloschema.ValueRefSQL("?1", "?2"), kind, tc.lang).Scan(&got))
		assert.Equal(t, tc.want, got, "SQL: %s/%s", tc.lang, tc.kind)
	}
}

// valueRefsSource uses functions only as values, passes 40 locals to one
// callee, calls 40 functions, and leaves two functions unused, so each rule's
// decision on value refs shows (mache-c5e2f9).
func valueRefsSource() map[string]string {
	var locals, calls, defs []string
	for i := 1; i <= 40; i++ {
		locals = append(locals, fmt.Sprintf("a%02d", i))
		calls = append(calls, fmt.Sprintf("\tf%02d()\n", i))
		defs = append(defs, fmt.Sprintf("func f%02d() {}\n", i))
	}
	zeros := strings.TrimSuffix(strings.Repeat("0, ", 40), ", ")
	return map[string]string{
		"a.go": "package a\n\n" +
			"func helper() int { return 1 }\n\nvar h = helper\n\n" +
			"func unused() int { return 2 }\n\n" +
			"func sink(...int) {}\n\n" +
			"func Wide() {\n\t" + strings.Join(locals, ", ") + " := " + zeros + "\n\tsink(" + strings.Join(locals, ", ") + ")\n}\n\n" +
			"func Busy() {\n" + strings.Join(calls, "") + "}\n\n" + strings.Join(defs, "") + "\n" +
			"func Exported() {}\n\nfunc Lonely() {}\n",
		"a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestUses(t *testing.T) {\n\tf := Exported\n\t_ = f\n}\n",
		"lib.rs":    "fn rhelper() -> i32 { 1 }\n\npub fn run() {\n    let f = rhelper;\n    let _ = f;\n}\n",
	}
}

// TestValueRefs_PerRuleDecisions is mache-c5e2f9 on a real parse: a value use
// is a use for dead_code and untested_function, and is not a receiver for
// fan_out_skew or a reference for RefsMap's most-referenced ranking.
func TestValueRefs_PerRuleDecisions(t *testing.T) {
	src := t.TempDir()
	for name, body := range valueRefsSource() {
		require.NoError(t, os.WriteFile(filepath.Join(src, name), []byte(body), 0o644))
	}
	db, _ := lltest.ParseSourceViaLeyline(t, src)
	var values int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM node_refs WHERE node_kind IN ('var_spec', 'short_var_declaration', 'let_declaration')`).Scan(&values))
	require.Positive(t, values, "the parse must hold value refs (leyline extraction epoch >= 6), or nothing here is tested")
	tg := &testutil.SmellTestGraph{DB: db}
	text := func(f smellFinding) string {
		b, err := os.ReadFile(filepath.Join(src, f.SourceID))
		require.NoError(t, err)
		return string(b[f.StartByte:f.EndByte])
	}
	starts := func(rule string, args map[string]any) []string {
		args["rule"] = rule
		var out []string
		for _, f := range runSmellRule(t, tg, args) {
			out = append(out, strings.SplitN(text(f), "(", 2)[0])
		}
		return out
	}

	dead := starts("dead_code", map[string]any{})
	assert.Contains(t, dead, "func unused", "a function nothing uses is dead")
	assert.NotContains(t, dead, "func helper", "dead_code: `var h = helper` is a use")
	assert.NotContains(t, dead, "fn rhelper", "dead_code: `let f = rhelper` is a use")

	untested := map[string]bool{}
	for _, f := range runSmellRule(t, tg, map[string]any{"rule": "untested_function"}) {
		untested[strings.SplitN(text(f), "(", 2)[0]] = true
	}
	assert.True(t, untested["func Lonely"], "an exported function no test touches is untested")
	assert.False(t, untested["func Exported"], "untested_function: a test taking it as a value uses it")

	fan := map[string]int64{}
	for _, f := range runSmellRule(t, tg, map[string]any{"rule": "fan_out_skew"}) {
		fan[f.NodeID] = f.Metric
	}
	require.Len(t, fan, 1, "only Busy talks to 35+ things: %v", fan)
	for _, metric := range fan {
		assert.Equal(t, int64(40), metric, "Busy calls 40 functions")
	}
	wide := runSmellRule(t, tg, map[string]any{"rule": "fan_out_skew", "min_metric": float64(0)})
	for _, f := range wide {
		assert.NotEqual(t, "func Wide", strings.SplitN(text(f), "(", 2)[0],
			"fan_out_skew: passing 40 locals to one callee is one receiver, not 41")
	}

	var dbPath string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
	g, err := graph.OpenSQLiteGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	refs := g.RefsMap()
	assert.Contains(t, refs, "sink", "a called function is referenced")
	assert.Contains(t, refs, "f01")
	for _, local := range []string{"a01", "a40", "helper", "rhelper", "Exported"} {
		assert.NotContains(t, refs, local, "RefsMap ranks what is called: %s is only a value", local)
	}
	callees := func(id string) []string {
		nodes, err := g.GetCallees(id)
		require.NoError(t, err)
		var out []string
		for _, n := range nodes {
			out = append(out, n.ID)
		}
		return out
	}
	assert.Empty(t, callees("lib.rs/function_item_1"), "find_callees: `let f = rhelper` is not a call of rhelper")
	assert.Empty(t, callees("a_test.go/function_declaration"), "find_callees: `f := Exported` is not a call of Exported")
	assert.Len(t, callees("a.go/function_declaration_4"), 40, "Busy calls f01..f40")

	count, err := g.RefTokenCount()
	require.NoError(t, err)
	assert.Equal(t, len(refs), count, "get_overview's ref_tokens counts what RefsMap holds")
}
