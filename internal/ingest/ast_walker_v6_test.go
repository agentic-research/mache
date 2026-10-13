package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/sqlcount"
)

// The walker reads ley-line-open's projection-v6 (integer nids, paths derived
// by LLO) into the same path-keyed file index it builds from projection-v4
// (mache-90cb9f). One spec is written as both shapes, and the index, the
// matches and the calls must come out identical.

type walkerSeed struct {
	name   string
	source string
	seed   func(*testing.T, fixturedb.Producer) *sql.DB
}

var walkerSeeds = []walkerSeed{
	// Each seed is called rather than named: a function referenced only as
	// a value reads as dead to the smell gate (ley-line-140307).
	{"go", "main.go", func(t *testing.T, p fixturedb.Producer) *sql.DB { return seedTestAST(t, p) }},
	{"hcl", "main.tf", func(t *testing.T, p fixturedb.Producer) *sql.DB { return seedHCLBlocksAST(t, p) }},
	{"nested", "pkg/sub/b.go", func(t *testing.T, p fixturedb.Producer) *sql.DB { return seedNested(t, p) }},
}

// seedNested is two files, the one indexed nested two directories down and
// numbered second, so its section must start after another file's and its
// root's parent is a directory rather than "".
func seedNested(t *testing.T, p fixturedb.Producer) *sql.DB {
	t.Helper()
	const src = "package sub\n\nfunc B() {}\n"
	b := fixturedb.New(t, p)
	b.Source("a.go", "go", "package main\n")
	b.ASTNode("a.go", "source_file", "a.go", fixturedb.Bytes(0, 13))
	b.ASTNode("a.go/package_clause", "package_clause", "a.go", fixturedb.Bytes(0, 12))
	b.Source("pkg/sub/b.go", "go", src)
	b.ASTNode("pkg/sub/b.go", "source_file", "pkg/sub/b.go", fixturedb.Bytes(0, len(src)))
	b.ASTNode("pkg/sub/b.go/package_clause", "package_clause", "pkg/sub/b.go", fixturedb.Bytes(0, 11))
	b.ASTNode("pkg/sub/b.go/function_declaration", "function_declaration", "pkg/sub/b.go", fixturedb.Bytes(13, 24))
	b.ASTNode("pkg/sub/b.go/function_declaration/identifier", "identifier", "pkg/sub/b.go",
		fixturedb.Bytes(18, 19), fixturedb.Detail{Token: "B", Field: "name"})
	_, f := b.Build()
	return f.DB()
}

func TestASTWalker_SameIndexFromProjectionV4AndV6(t *testing.T) {
	for _, s := range walkerSeeds {
		t.Run(s.name, func(t *testing.T) {
			index := func(p fixturedb.Producer) []idxNode {
				w := NewASTWalker(s.seed(t, p))
				idx, err := w.fileIndex(s.source)
				require.NoError(t, err)
				return idx.all
			}
			v4 := index(fixturedb.Leyline)
			require.Greater(t, len(v4), 2, "the v4 index must hold the file, or the comparison is vacuous")
			assert.Equal(t, v4, index(fixturedb.LeylineV6))
		})
	}
}

// matchLines renders every match of selector under root, and every match of
// the nested selector scoped to each, as sorted lines a comparison can read.
func matchLines(t *testing.T, w *ASTWalker, root ASTRoot, selector, nested string) []string {
	t.Helper()
	ms, err := w.Query(root, selector)
	require.NoError(t, err)
	var out []string
	for _, m := range ms {
		am := m.(*astMatch)
		out = append(out, fmt.Sprintf("%s [%d,%d) %v %v", am.ctx.ParentPrefix, am.startByte, am.endByte,
			am.values, am.captureRanges))
		if nested == "" {
			continue
		}
		inner, err := w.Query(am.ctx, nested)
		require.NoError(t, err)
		for _, im := range inner {
			ia := im.(*astMatch)
			out = append(out, fmt.Sprintf("  %s [%d,%d) %v", ia.ctx.ParentPrefix, ia.startByte, ia.endByte, ia.values))
		}
	}
	sort.Strings(out)
	return out
}

func TestASTWalker_SameMatchesFromProjectionV4AndV6(t *testing.T) {
	queries := map[string][2]string{
		"go": {
			"(function_declaration name: (identifier) @name) @scope",
			"(parameter_declaration name: (identifier) @param type: (type_identifier) @type) @scope",
		},
		"hcl": {
			`(block (identifier) @kind (#eq? @kind "resource")) @scope`,
			"(body) @scope",
		},
		"nested": {"(function_declaration name: (identifier) @name) @scope", ""},
	}
	for _, s := range walkerSeeds {
		t.Run(s.name, func(t *testing.T) {
			q := queries[s.name]
			lines := func(p fixturedb.Producer) []string {
				db := s.seed(t, p)
				return matchLines(t, NewASTWalker(db), ASTRoot{DB: db, SourceID: s.source}, q[0], q[1])
			}
			v4 := lines(fixturedb.Leyline)
			require.NotEmpty(t, v4, "the v4 side must match, or the comparison is vacuous")
			assert.Equal(t, v4, lines(fixturedb.LeylineV6))
		})
	}
}

// On v6 the walker adds no index to LLO's table: a file is a primary-key range
// of _ast already, and EnsureIndexes says so by doing nothing.
func TestASTWalker_EnsureIndexesWritesNothingOnV6(t *testing.T) {
	db := seedTestAST(t, fixturedb.LeylineV6)
	var before, after int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index'`).Scan(&before))
	require.NoError(t, NewASTWalker(db).EnsureIndexes())
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index'`).Scan(&after))
	assert.Equal(t, before, after)
}

// FCA inference reads _ast through lloschema's ASTSelect, so the records are
// the same from either shape.
func TestFlattenASTDB_SameRecordsFromProjectionV4AndV6(t *testing.T) {
	flat := func(p fixturedb.Producer) []string {
		recs, err := FlattenASTDB(seedTestAST(t, p), "", 0)
		require.NoError(t, err)
		out := make([]string, 0, len(recs))
		for _, r := range recs {
			out = append(out, fmt.Sprint(r))
		}
		return out
	}
	v4 := flat(fixturedb.Leyline)
	require.Greater(t, len(v4), 5)
	assert.Equal(t, v4, flat(fixturedb.LeylineV6), "order included: FlattenAST's preorder")
}

// Loading one file's section costs the same in a database 200 times larger:
// the same statements, and file reads that grow with b-tree depth, not size.
func TestASTWalker_V6SectionCostDoesNotGrowWithTheDatabase(t *testing.T) {
	small := fixturedb.Wide(t, fixturedb.LeylineV6, 10).DBPath()
	large := fixturedb.Wide(t, fixturedb.LeylineV6, 2000).DBPath()
	load := func(w *ASTWalker) {
		idx, err := w.fileIndex("pkg/f0000.go")
		require.NoError(t, err)
		require.Len(t, idx.byKind["function_declaration"], 3)
	}

	statements := func(path string) int64 {
		sqlcount.RegisterDriver()
		db, err := sql.Open(sqlcount.DriverName, path+"?mode=ro")
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		w := NewASTWalker(db)
		w.isV6() // the shape probe is per walker, not per file
		count := sqlcount.Reset()
		load(w)
		return count()
	}
	require.Positive(t, statements(small))
	assert.Equal(t, statements(small), statements(large))

	bytesRead := func(path string) int64 {
		rc, err := sqlcount.OpenReadCounted(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, rc.Close()) }()
		w := NewASTWalker(rc.DB)
		n := rc.Reset()
		load(w)
		return n()
	}
	s, l := bytesRead(small), bytesRead(large)
	require.Positive(t, s)
	assert.LessOrEqual(t, l, 4*s, "read %d bytes at 10 files and %d at 2000: cost grows with the database", s, l)
}

// Every file of a real `leyline parse` loads, and its index is consistent: the
// root is the file, every other node sits under a parent the index holds, and
// every id is its parent's plus a segment. Set MACHE_TEST_LEYLINE_V6_DB to a
// projection-v6 .db to run it.
func TestASTWalker_LoadsEveryFileOfARealProjectionV6(t *testing.T) {
	path := os.Getenv("MACHE_TEST_LEYLINE_V6_DB")
	if path == "" {
		t.Skip("MACHE_TEST_LEYLINE_V6_DB not set")
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT id FROM _source ORDER BY id`)
	require.NoError(t, err)
	var sources []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		sources = append(sources, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, sources)

	w := NewASTWalker(db)
	total := 0
	for _, src := range sources {
		idx, err := w.fileIndex(src)
		require.NoError(t, err, src)
		require.NotEmpty(t, idx.all, src)
		for _, n := range idx.all {
			if n.id == src {
				continue
			}
			_, ok := idx.byID[n.parentID]
			require.True(t, ok, "%s: %s's parent %s is not in the index", src, n.id, n.parentID)
			require.Equal(t, n.parentID+"/"+n.name, n.id, src)
		}
		total += len(idx.all)
		w.InvalidateSource(src)
	}
	t.Logf("loaded %d files, %d located nodes", len(sources), total)
}
