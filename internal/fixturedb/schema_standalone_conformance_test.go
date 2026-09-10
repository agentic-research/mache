package fixturedb

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The other derivation, re-run — and unlike the ley-line one this needs no
// binary, so it is UNGATED and runs on every `go test ./...`.
//
// It reads the DDL NewSQLiteWriter executes out of internal/ingest/sqlite_writer.go's
// Go AST — the non-literal `db.Exec(...)` argument, with each identifier resolved
// to its package-level string constant — executes it into a scratch database,
// and diffs sqlite_master against [standaloneTables] / [standaloneIndexes] /
// [standaloneViews] in BOTH directions: every object the fixture models must
// match the writer, and every object the writer creates must be modelled. The
// second direction is what catches an index added to the writer and forgotten
// here — a fixture missing it plans queries differently from a real .db.
//
// Reading the AST rather than importing internal/ingest is deliberate twice
// over: internal/ingest pulls tree-sitter and therefore CGO, which fixtures must
// never require; and matching the file as text would need a regexp, which this
// repo ratchets against (internal/lint's regexpAllowlist).
func TestStandaloneSchema_MatchesSQLiteWriter(t *testing.T) {
	got := deriveWriterSchema(t)

	var modelled []string
	for _, m := range []map[string]string{standaloneTables, standaloneIndexes, standaloneViews} {
		modelled = append(modelled, slices.Sorted(maps.Keys(m))...)
	}
	assert.ElementsMatch(t, modelled, sortedNames(got),
		"objects created by ingest.SQLiteWriter and objects modelled by the Standalone fixture differ")

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

// deriveWriterSchema evaluates the DDL expression NewSQLiteWriter hands to
// db.Exec and runs it.
func deriveWriterSchema(t *testing.T) map[string]string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	writerPath := filepath.Join(repoRoot, "internal", "ingest", "sqlite_writer.go")

	file, err := parser.ParseFile(token.NewFileSet(), writerPath, nil, 0)
	require.NoError(t, err, "parse %s", writerPath)

	consts := map[string]string{}
	for _, decl := range file.Decls {
		gd, isGen := decl.(*ast.GenDecl)
		if !isGen || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if lit, isLit := vs.Values[i].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
					unquoted, uerr := strconv.Unquote(lit.Value)
					require.NoError(t, uerr)
					consts[name.Name] = unquoted
				}
			}
		}
	}

	var ddl []string
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Name.Name != "NewSQLiteWriter" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || len(call.Args) != 1 {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "Exec" {
				return true
			}
			// PRAGMAs and the ALTERs are literals; the schema is the one
			// expression built from named constants.
			if _, isLit := call.Args[0].(*ast.BasicLit); isLit {
				return true
			}
			ddl = append(ddl, evalStringExpr(t, call.Args[0], consts))
			return true
		})
	}
	require.Len(t, ddl, 1,
		"expected exactly one non-literal db.Exec(...) in NewSQLiteWriter in %s — if the "+
			"writer was restructured, update this derivation rather than snapshotting its output",
		writerPath)

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "writer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(ddl[0])
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

// evalStringExpr resolves a string expression made of literals, package-level
// string constants and `+` concatenation — the grammar NewSQLiteWriter's
// schema argument is written in.
func evalStringExpr(t *testing.T, e ast.Expr, consts map[string]string) string {
	t.Helper()
	switch x := e.(type) {
	case *ast.BasicLit:
		require.Equal(t, token.STRING, x.Kind)
		v, err := strconv.Unquote(x.Value)
		require.NoError(t, err)
		return v
	case *ast.Ident:
		v, ok := consts[x.Name]
		require.True(t, ok, "%s is not a package-level string constant", x.Name)
		return v
	case *ast.BinaryExpr:
		require.Equal(t, token.ADD, x.Op)
		return evalStringExpr(t, x.X, consts) + evalStringExpr(t, x.Y, consts)
	case *ast.ParenExpr:
		return evalStringExpr(t, x.X, consts)
	}
	require.Failf(t, "unsupported expression", "%T in NewSQLiteWriter's schema argument", e)
	return ""
}
