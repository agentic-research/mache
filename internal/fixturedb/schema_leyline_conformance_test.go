package fixturedb

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The derivation, re-run.
//
// [leylineTables] and [leylineIndexes] claim to be the pinned producer's own
// `sqlite_master` output. This test makes that claim FALSIFIABLE: it runs the
// pinned binary on a two-file corpus and diffs, so a ley-line release that
// changes shape fails here — instead of silently flipping which arm of
// ensureCanonicalViews the entire suite exercises, which is exactly how the
// pre-fixturedb fixtures drifted to ten mutually incompatible spellings without
// anyone noticing.
//
// GATED, never downloads: skips when no cached binary matches the pin, matching
// every other pinned gate in this repo.
// Set leyline.BinaryOverrideEnv (MACHE_LEYLINE_BINARY) to run this against a release
// candidate or an unmerged local build instead of the cached pin. Under an
// override a DIFF IS THE POINT: it is the re-derivation worklist for that
// candidate, enumerated before either side ships, rather than a regression.
// See mache-cc1a70.
func TestLeylineSchema_MatchesPinnedBinary(t *testing.T) {
	ll := lltest.ResolveBinaryOrSkip(t)

	if ll.Override {
		// Asserting "the DDL was derived from the pin" is meaningless when the
		// operator deliberately pointed us at something else. Report what
		// actually answered, so no reader mistakes this run for a pinned one.
		t.Logf("comparing schema_leyline.go (derived from %s) against OVERRIDE %s reporting %s — "+
			"differences below are that candidate's re-derivation worklist, not drift from the pin",
			leylineSchemaVersion, ll.Path, ll.Version)
	} else {
		require.Equal(t, leyline.PinnedBinaryVersion(), leylineSchemaVersion,
			"the DDL in schema_leyline.go was derived from %s but the build now pins %s — "+
				"re-derive it (leyline parse; SELECT sql FROM sqlite_master) rather than editing by hand",
			leylineSchemaVersion, leyline.PinnedBinaryVersion())
	}

	got := derivePinnedSchema(t, ll.Path)
	against := leylineSchemaVersion
	if ll.Override {
		against = ll.Version + " (override)"
	}

	for name, want := range leylineTables {
		g, ok := got[name]
		require.True(t, ok, "leyline %s no longer creates table %s", against, name)
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g),
			"table %s drifted from producer %s; re-derive schema_leyline.go", name, against)
	}
	for name, want := range leylineIndexes {
		g, ok := got[name]
		require.True(t, ok, "leyline %s no longer creates index %s", against, name)
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g),
			"index %s drifted from producer %s; re-derive schema_leyline.go", name, against)
	}
}

// TestLeylineSchema_NodeRefsHasNoPrimaryKey pins the property that made 13
// pre-fixturedb fixtures wrong in a way no column diff would show: ley-line's
// node_refs has NO primary key, so duplicate (token, node_id) rows SURVIVE.
// Every fixture that added `PRIMARY KEY (token, node_id) WITHOUT ROWID` deduped
// rows production keeps, so any COUNT/AVG-over-v_refs rule measured a different
// quantity in test than in prod (the fan_out_skew class, mache-50e939).
func TestLeylineSchema_NodeRefsHasNoPrimaryKey(t *testing.T) {
	for _, tbl := range []string{"node_refs", "node_defs"} {
		ddl := strings.ToUpper(leylineTables[tbl])
		assert.NotContains(t, ddl, "PRIMARY KEY", "%s: ley-line has no primary key here", tbl)
		assert.NotContains(t, ddl, "WITHOUT ROWID", "%s: ley-line keeps the rowid here", tbl)
	}

	// And the behaviour that follows from it, end to end.
	b := New(t, Leyline)
	b.Construct("a.go/function_declaration_0", Where{Source: "a.go"})
	b.Ref("Println", "a.go/function_declaration_0", "a.go/call_0", "fmt")
	b.Ref("Println", "a.go/function_declaration_0", "a.go/call_0", "fmt")
	_, f := b.Build()

	var n int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM node_refs WHERE token='Println'`).Scan(&n))
	assert.Equal(t, 2, n, "ley-line keeps duplicate occurrences; a primary key would hide one")
}

// TestStandaloneSchema_NodeRefsDedupes is the mirror: the mache projection DOES
// collapse them, and a test must be able to see that difference rather than
// inherit whichever one its hand-written DDL happened to encode.
func TestStandaloneSchema_NodeRefsDedupes(t *testing.T) {
	b := New(t, Standalone)
	b.Ref("Println", "pkg/functions/Run", "", "fmt")
	b.Ref("Println", "pkg/functions/Run", "", "fmt")
	_, f := b.Build()

	var n int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM node_refs WHERE token='Println'`).Scan(&n))
	assert.Equal(t, 1, n, "PRIMARY KEY (token, node_id) collapses the two occurrences")
}

// derivePinnedSchema runs the pinned binary on a tiny corpus and returns
// sqlite_master keyed by object name.
func derivePinnedSchema(t *testing.T, bin string) map[string]string {
	t.Helper()

	// /tmp rather than t.TempDir(): the parse writes a .db next to a corpus and
	// deep nesting has bitten the sibling helpers here before.
	dir, err := os.MkdirTemp("/tmp", "fxdbderive")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	// Two languages and a qualified call, so every modelled table gets rows:
	// _imports needs an import, node_refs needs a call, node_content needs
	// tokens.
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"),
		[]byte("package a\n\nimport \"fmt\"\n\nfunc Alpha() { fmt.Println(\"hi\") }\n\nfunc Beta() { Alpha() }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.py"),
		[]byte("def gamma():\n    return 1\n"), 0o644))

	out := filepath.Join(dir, "out.db")
	cmd := exec.Command(bin, "parse", src, "-o", out)
	if combined, cerr := cmd.CombinedOutput(); cerr != nil {
		t.Fatalf("fixturedb: leyline parse failed: %v\n%s", cerr, combined)
	}

	db, err := sql.Open("sqlite", out)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	got := map[string]string{}
	for rows.Next() {
		var name, ddl string
		require.NoError(t, rows.Scan(&name, &ddl))
		got[name] = ddl
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, got, "leyline parse produced no schema")
	return got
}

// normalizeDDL collapses formatting so the comparison catches SHAPE changes
// (columns, constraints, WITHOUT ROWID) and not indentation. SQLite stores `--`
// comments verbatim in sqlite_master.sql, and mache's own writer embeds long
// ones, so they are stripped too.
func normalizeDDL(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

// sortedNames is a test-local helper for stable failure output.
func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
