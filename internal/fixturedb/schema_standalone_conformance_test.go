package fixturedb

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The other derivation, re-run — and unlike the ley-line one this needs no
// binary, so it is UNGATED and runs on every `go test ./...`.
//
// It reads internal/ingest/sqlite_writer.go's own schema literal out of the Go
// AST, executes it into a scratch database, and diffs sqlite_master against
// [standaloneTables] / [standaloneIndexes] / [standaloneViews].
//
// Reading the AST rather than importing internal/ingest is deliberate twice
// over: internal/ingest pulls tree-sitter and therefore CGO, which fixtures must
// never require; and matching the file as text would need a regexp, which this
// repo ratchets against (internal/lint's regexpAllowlist).
func TestStandaloneSchema_MatchesSQLiteWriter(t *testing.T) {
	got := deriveWriterSchema(t)

	for name, want := range standaloneTables {
		g, ok := got[name]
		require.True(t, ok,
			"ingest.SQLiteWriter no longer creates table %s (it creates %v)", name, sortedNames(got))
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g),
			"table %s drifted from internal/ingest/sqlite_writer.go", name)
	}
	for name, want := range standaloneIndexes {
		g, ok := got[name]
		require.True(t, ok, "ingest.SQLiteWriter no longer creates index %s", name)
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g), "index %s drifted", name)
	}
	for name, want := range standaloneViews {
		g, ok := got[name]
		require.True(t, ok, "ingest.SQLiteWriter no longer creates view %s", name)
		assert.Equal(t, normalizeDDL(want), normalizeDDL(g), "view %s drifted", name)
	}
}

// TestStandaloneSchema_HasNoProducerTables pins the boundary the other way: the
// mache projection writes NONE of ley-line's parse output. A Standalone fixture
// that silently grew an `_ast` table would flip ensureCanonicalViews onto the
// v_test_nodes arm that a real mache .db never reaches.
func TestStandaloneSchema_HasNoProducerTables(t *testing.T) {
	for _, tbl := range []string{"_ast", "_source", "node_content", "_imports"} {
		_, ok := standaloneTables[tbl]
		assert.False(t, ok, "%s is ley-line-owned; the mache projection does not write it", tbl)
	}

	b := New(t, Standalone)
	b.Def("Run", "pkg/functions/Run", Function)
	_, f := b.Build()

	var n int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name='_ast'`).Scan(&n))
	assert.Zero(t, n, "a Standalone fixture with no AST rows must have no _ast table")
}

// deriveWriterSchema pulls the schema literal out of NewSQLiteWriter and runs it.
func deriveWriterSchema(t *testing.T) map[string]string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	writerPath := filepath.Join(repoRoot, "internal", "ingest", "sqlite_writer.go")

	file, err := parser.ParseFile(token.NewFileSet(), writerPath, nil, 0)
	require.NoError(t, err, "parse %s", writerPath)

	var ddl string
	ast.Inspect(file, func(n ast.Node) bool {
		asn, isAssign := n.(*ast.AssignStmt)
		if !isAssign || len(asn.Lhs) != 1 || len(asn.Rhs) != 1 {
			return true
		}
		ident, isIdent := asn.Lhs[0].(*ast.Ident)
		if !isIdent || ident.Name != "schema" {
			return true
		}
		lit, isLit := asn.Rhs[0].(*ast.BasicLit)
		if !isLit || lit.Kind != token.STRING {
			return true
		}
		unquoted, uerr := strconv.Unquote(lit.Value)
		require.NoError(t, uerr)
		ddl = unquoted
		return false
	})
	require.NotEmpty(t, ddl,
		"could not find the `schema := ...` literal in %s — if the writer was "+
			"restructured, update this derivation rather than snapshotting its output", writerPath)

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "writer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(ddl)
	require.NoError(t, err)

	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var name, s string
		require.NoError(t, rows.Scan(&name, &s))
		out[name] = s
	}
	require.NoError(t, rows.Err())
	return out
}
