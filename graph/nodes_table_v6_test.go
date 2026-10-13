package graph

import (
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/sqlcount"
)

// The nodes-table reader reads ley-line-open's projection-v6 (integer nids,
// derived paths) through the same methods it reads path-keyed tables with
// (mache-90cb9f). The spec below is built as both shapes, and a consumer must
// get the same answers from each.

func readerSpec(t *testing.T, p fixturedb.Producer) *NodesTableReader {
	b := fixturedb.New(t, p)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go",
		fixturedb.Span{StartByte: 42, EndByte: 82, StartRow: 4, StartCol: 0, EndRow: 6, EndCol: 1})
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 40, EndByte: 110, StartRow: 3, StartCol: 0, EndRow: 8, EndCol: 1})
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go",
		fixturedb.Span{StartByte: 112, EndByte: 142, StartRow: 10, StartCol: 0, EndRow: 12, EndCol: 1})
	b.ASTNode("pkg/a.go/function_declaration_0/identifier", "identifier", "pkg/a.go",
		fixturedb.Bytes(45, 50), fixturedb.Detail{Token: "Greet"})
	// A token leaf, as a real parse writes one: no children, its text in
	// nodes.record. It is the node whose content the comparison reads.
	b.Construct("pkg/a.go/function_declaration_0/identifier", fixturedb.Where{Leaf: true})
	// The call site is declared as a node too: a real parse has a nodes row
	// for it on either shape, and the v4 fixture writes one only when asked.
	b.ASTNode("main.go/function_declaration/call_expression", "call_expression", "main.go",
		fixturedb.Bytes(60, 70))
	b.Def("Greet", "pkg/a.go/function_declaration_0", fixturedb.Function)
	b.Def("helper", "pkg/a.go/function_declaration_1", fixturedb.Function)
	b.Ref("Greet", "main.go/function_declaration", "main.go/function_declaration/call_expression", "pkg")
	_, f := b.Build()
	r := NewNodesTableReader(f.DB(), "nodes", nil, nil, 0o444, 0o555, 16)
	t.Cleanup(r.Close)
	return r
}

var readerDeclared = []string{
	"main.go/function_declaration",
	"main.go/function_declaration/call_expression",
	"pkg/a.go/function_declaration_0",
	"pkg/a.go/function_declaration_1",
	"pkg/a.go/function_declaration_0/identifier",
}

// view is what a consumer reads about one node.
type view struct {
	Dir     bool
	Origin  *SourceOrigin
	Len     int64
	Content string
	Kids    []string
}

func viewOf(t *testing.T, r *NodesTableReader, id string) view {
	t.Helper()
	n, err := r.GetNode(id)
	require.NoError(t, err, id)
	v := view{Dir: n.Mode.IsDir(), Origin: n.Origin}
	if n.Ref != nil {
		v.Len = n.Ref.ContentLen
	}
	if !v.Dir {
		buf := make([]byte, 4096)
		k, err := r.ReadContent(id, buf, 0)
		if err == nil {
			v.Content = string(buf[:k])
		}
	}
	v.Kids, err = r.ListChildren(id)
	require.NoError(t, err, id)
	return v
}

func TestNodesTableReader_SameAnswersFromProjectionV4AndV6(t *testing.T) {
	v4, v6 := readerSpec(t, fixturedb.Leyline), readerSpec(t, fixturedb.LeylineV6)
	require.Nil(t, v4.shape, "a projection-v4 table is path-keyed")
	require.NotNil(t, v6.shape, "a projection-v6 table is nid-keyed")

	for _, id := range readerDeclared {
		want := viewOf(t, v4, id)
		require.NotNil(t, want.Origin, "%s: the v4 side must be locatable, or the comparison is vacuous", id)
		assert.Equal(t, want, viewOf(t, v6, id), id)
	}
	leaf := viewOf(t, v4, "pkg/a.go/function_declaration_0/identifier")
	require.Equal(t, "Greet", leaf.Content, "the leaf's content must be read, or its comparison is vacuous")
}

func TestNodesTableReader_V6ListsTheTreeLLODerives(t *testing.T) {
	r := readerSpec(t, fixturedb.LeylineV6)

	root, err := r.ListChildren("")
	require.NoError(t, err)
	assert.Equal(t, []string{"main.go", "pkg"}, root)

	pkg, err := r.ListChildren("pkg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/a.go"}, pkg)

	file, err := r.ListChildren("pkg/a.go")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/a.go/function_declaration_0", "pkg/a.go/function_declaration_1"}, file)

	stats, err := r.ListChildStats("")
	require.NoError(t, err)
	ids := make([]string, 0, len(stats))
	for _, s := range stats {
		ids = append(ids, s.ID)
		assert.True(t, s.IsDir, "%s: a directory and a file with children both list as directories", s.ID)
	}
	assert.Equal(t, root, ids, "a stat walk and an id walk must see the same tree")

	dir, err := r.GetNode("pkg")
	require.NoError(t, err)
	assert.True(t, dir.Mode.IsDir())
	assert.Nil(t, dir.Origin, "a directory has no _ast row, so no location")

	fn, err := r.GetNode("pkg/a.go/function_declaration_1")
	require.NoError(t, err)
	require.NotNil(t, fn.Origin)
	assert.Equal(t, "pkg/a.go", fn.Origin.FilePath, "the file comes from the nid's file_id, through _source")
	assert.Equal(t, uint32(11), fn.Origin.StartLine)
}

func TestNodesTableReader_V6Misses(t *testing.T) {
	r := readerSpec(t, fixturedb.LeylineV6)

	_, err := r.GetNode("pkg/nope.go")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = r.ReadContent("pkg/a.go/function_declaration", make([]byte, 8), 0)
	assert.ErrorIs(t, err, ErrNotFound, "a repeated kind is never bare")

	kids, err := r.ListChildren("nope")
	require.NoError(t, err)
	assert.Nil(t, kids, "a node that does not exist lists nothing, as on a path-keyed table")
	stats, err := r.ListChildStats("nope")
	require.NoError(t, err)
	assert.Nil(t, stats)
}

// A caller is the reference's site, as projection-v4's node_refs.node_id is.
func TestNodesTableReader_V6CallersAreSitePaths(t *testing.T) {
	v4, v6 := readerSpec(t, fixturedb.Leyline), readerSpec(t, fixturedb.LeylineV6)
	ids := func(r *NodesTableReader) []string {
		nodes, err := r.GetCallers("Greet")
		require.NoError(t, err)
		var out []string
		for _, n := range nodes {
			out = append(out, n.ID)
		}
		return out
	}
	want := ids(v4)
	require.Equal(t, []string{"main.go/function_declaration/call_expression"}, want)
	assert.Equal(t, want, ids(v6))

	none, err := v6.GetCallers("nobody")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// Reading a node must not cost more in a bigger database: statements per read
// are fixed, and the bytes read grow with b-tree depth, not table size. See
// lloschema's TestV6Paths_LookupCostDoesNotGrowWithTheDatabase for why both
// units, and for the measured separation the 4x bound sits in.
func TestNodesTableReader_V6ReadCostDoesNotGrowWithTheDatabase(t *testing.T) {
	small := fixturedb.Wide(t, fixturedb.LeylineV6, 10).DBPath()
	large := fixturedb.Wide(t, fixturedb.LeylineV6, 2000).DBPath()
	const parent = "pkg/f0000.go/function_declaration_1"

	read := func(r *NodesTableReader) {
		n, err := r.GetNode(fixturedb.WideDeepID)
		require.NoError(t, err)
		require.NotNil(t, n.Origin)
		kids, err := r.ListChildren(parent)
		require.NoError(t, err)
		require.Equal(t, []string{fixturedb.WideDeepID}, kids)
	}

	statements := func(path string) int64 {
		sqlcount.RegisterDriver()
		db, err := sql.Open(sqlcount.DriverName, path+"?mode=ro")
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 16)
		defer r.Close()
		read(r) // compiling is a one-off
		count := sqlcount.Reset()
		read(r)
		return count()
	}
	require.Positive(t, statements(small))
	assert.Equal(t, statements(small), statements(large), "statements per read must not grow with the database")

	bytesRead := func(path string) int64 {
		rc, err := sqlcount.OpenReadCounted(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, rc.Close()) }()
		r := NewNodesTableReader(rc.DB, "nodes", nil, nil, 0o444, 0o555, 16)
		defer r.Close()
		n := rc.Reset()
		read(r)
		return n()
	}
	s, l := bytesRead(small), bytesRead(large)
	require.Positive(t, s)
	assert.LessOrEqual(t, l, 4*s, "read %d bytes at 10 files and %d at 2000: cost grows with the database", s, l)
}

// A walk of a real `leyline parse` output: every node listed is readable, its
// stat agrees with GetNode, and a located node's file is the file it sits
// under. Set MACHE_TEST_LEYLINE_V6_DB to a projection-v6 .db to run it.
func TestNodesTableReader_WalksARealProjectionV6(t *testing.T) {
	path := os.Getenv("MACHE_TEST_LEYLINE_V6_DB")
	if path == "" {
		t.Skip("MACHE_TEST_LEYLINE_V6_DB not set")
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 16)
	defer r.Close()
	require.NotNil(t, r.shape, "a v0.20.x parse is projection-v6")

	var total int64
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM nodes WHERE nid <> -1`).Scan(&total))

	seen := int64(0)
	queue := []string{""}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		stats, err := r.ListChildStats(id)
		require.NoError(t, err, id)
		kids, err := r.ListChildren(id)
		require.NoError(t, err, id)
		ids := make([]string, 0, len(stats))
		for _, s := range stats {
			ids = append(ids, s.ID)
			n, err := r.GetNode(s.ID)
			require.NoError(t, err, s.ID)
			require.Equal(t, s.IsDir, n.Mode.IsDir(), s.ID)
			if n.Origin != nil {
				require.True(t, strings.HasPrefix(s.ID, n.Origin.FilePath+"/") || s.ID == n.Origin.FilePath,
					"%s is located in %s", s.ID, n.Origin.FilePath)
			}
			queue = append(queue, s.ID)
		}
		require.True(t, slices.Equal(kids, ids), "%s: ListChildren and ListChildStats disagree", id)
		seen += int64(len(stats))
	}
	assert.Equal(t, total, seen, "the walk from the root must reach every node exactly once")
	t.Logf("walked %d nodes", seen)
}
