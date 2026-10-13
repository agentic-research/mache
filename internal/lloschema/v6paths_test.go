package lloschema

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/sqlcount"
)

// v6PathsSpec is a projection-v6 fixture with every shape the addressing has
// to get right: nested directories, a file at the root, a kind that repeats
// (and so is suffixed) next to one that does not, and nesting below both.
func v6PathsSpec(t *testing.T) *fixturedb.FixtureDB {
	b := fixturedb.New(t, fixturedb.LeylineV6)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.Source("pkg/sub/b.go", "go", "")
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go", fixturedb.Bytes(42, 82))
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go", fixturedb.Bytes(40, 110))
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go", fixturedb.Bytes(112, 142))
	b.ASTNode("pkg/a.go/function_declaration_0/identifier", "identifier", "pkg/a.go", fixturedb.Bytes(45, 50))
	b.ASTNode("pkg/a.go/function_declaration_1/block", "block", "pkg/a.go", fixturedb.Bytes(120, 142))
	b.ASTNode("pkg/sub/b.go/type_declaration", "type_declaration", "pkg/sub/b.go", fixturedb.Bytes(10, 30))
	_, f := b.Build()
	return f
}

// allNodes is every nid in the database with the path LLO's own v_node_path
// renders for it: the oracle. v_node_path is the slow, whole-arena view the
// addressing exists to avoid, which is fine for a test over a small fixture.
func allNodes(t *testing.T, db *sql.DB) map[int64]string {
	rows, err := db.Query(`SELECT nid, path FROM v_node_path`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var nid int64
		var p string
		require.NoError(t, rows.Scan(&nid, &p))
		out[nid] = p
	}
	require.NoError(t, rows.Err())
	return out
}

func TestV6Paths_AgreeWithLLOsViewOnEveryNode(t *testing.T) {
	f := v6PathsSpec(t)
	want := allNodes(t, f.DB())
	p := NewV6Paths(f.DB())
	defer p.Close()

	require.Len(t, want, 12, "root, 2 directories, 3 files and 6 syntax nodes")
	for nid, path := range want {
		got, ok, err := p.Path(nid)
		require.NoError(t, err)
		require.True(t, ok, "nid %d", nid)
		assert.Equal(t, path, got, "Path(%d)", nid)

		back, ok, err := p.Resolve(path)
		require.NoError(t, err)
		require.True(t, ok, "Resolve(%q)", path)
		assert.Equal(t, nid, back, "Resolve(%q)", path)
	}
}

func TestV6Paths_Misses(t *testing.T) {
	f := v6PathsSpec(t)
	p := NewV6Paths(f.DB())
	defer p.Close()

	for _, path := range []string{
		"nope",                            // no such directory or file
		"pkg/nope.go",                     // no such file in a real directory
		"pkg/a.go/nope",                   // no such syntax child
		"pkg/a.go/identifier",             // a real kind, but not a child here
		"pkg/a.go/function_declaration",   // a repeated kind is never bare
		"pkg/a.go/function_declaration_2", // past the last sibling of that kind
		"pkg/a.go/function_declaration_x", // a suffix that is not a rank
		"pkg/a.go/function_declaration_",  // an empty suffix
		"pkg/a.go/function_declaration_99999999999999999999", // overflows int64
		"main.go/function_declaration/identifier/deeper",
		"pkg/sub/b.go/type_declaration/nope",
	} {
		nid, ok, err := p.Resolve(path)
		require.NoError(t, err, path)
		assert.False(t, ok, "%q must not resolve, got nid %d", path, nid)
	}

	_, ok, err := p.Path(1 << 40)
	require.NoError(t, err)
	assert.False(t, ok, "a nid with no row has no path")
}

// LLO resolves a "_k" suffix on a kind that does not repeat to the k-th
// sibling anyway. The addressing matches it: a path resolves here exactly when
// it resolves in LLO, or mache and a mount over the same db would disagree.
func TestV6Paths_ASuffixOnASingletonResolvesAsLLODoes(t *testing.T) {
	f := v6PathsSpec(t)
	p := NewV6Paths(f.DB())
	defer p.Close()

	bare, ok, err := p.Resolve("main.go/function_declaration")
	require.NoError(t, err)
	require.True(t, ok)
	suffixed, ok, err := p.Resolve("main.go/function_declaration_0")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, bare, suffixed)
}

func TestV6Paths_RootAndDirectories(t *testing.T) {
	f := v6PathsSpec(t)
	p := NewV6Paths(f.DB())
	defer p.Close()

	root, ok, err := p.Resolve("")
	require.NoError(t, err)
	require.True(t, ok)
	path, ok, err := p.Path(root)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "", path, "the root renders as the empty path")

	sub, ok, err := p.Resolve("pkg/sub")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Less(t, sub, int64(0), "a directory is a negative nid")
}

func TestSplitRank(t *testing.T) {
	for _, tc := range []struct {
		seg, kind string
		rank      int64
		ok        bool
	}{
		{"function_declaration_2", "function_declaration", 2, true},
		{"expression_statement_0", "expression_statement", 0, true},
		{"block", "", 0, false},
		{"call_", "", 0, false},
		{"call_+3", "", 0, false},
		{"call_-3", "", 0, false},
		{"call_1x", "", 0, false},
		{"_7", "", 7, true},
		{"x_99999999999999999999", "", 0, false},
	} {
		kind, rank, ok := splitRank(tc.seg)
		assert.Equal(t, tc.ok, ok, tc.seg)
		if tc.ok {
			assert.Equal(t, tc.kind, kind, tc.seg)
			assert.Equal(t, tc.rank, rank, tc.seg)
		}
	}
}

// The reason this type exists: v_node_path's cost grows with the database, so
// a lookup through it gets slower as the repository does. Cost here is
// measured in two units a slow runner cannot fake, between databases 200 times
// apart in size:
//
//   - statements per lookup, which catches a lookup that becomes a loop, and
//     must not change at all;
//   - bytes of the file read per lookup, which catches ONE statement doing a
//     whole table's work. An indexed lookup reads a few pages per b-tree level,
//     and levels grow with log(n); a scan reads the table, and grows with n.
//     Measured, v_node_path's reads grow ~1000x over this range while these
//     grow under 2x, so the bound below separates the two classes with room on
//     both sides.
const deepPath = fixturedb.WideDeepID

func TestV6Paths_LookupCostDoesNotGrowWithTheDatabase(t *testing.T) {
	small, large := fixturedb.Wide(t, fixturedb.LeylineV6, 10).DBPath(), fixturedb.Wide(t, fixturedb.LeylineV6, 2000).DBPath()

	statements := func(path string) int64 {
		sqlcount.RegisterDriver()
		db, err := sql.Open(sqlcount.DriverName, path+"?mode=ro")
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		p := NewV6Paths(db)
		defer p.Close()
		roundTrip(t, p) // compiling a statement is a one-off, not per-lookup work
		count := sqlcount.Reset()
		roundTrip(t, p)
		return count()
	}
	require.Positive(t, statements(small), "the counter must see the lookups, or the gate is vacuous")
	assert.Equal(t, statements(small), statements(large), "statements per lookup must not grow with the database")

	// Each direction cold, on its own connection, so neither is measured with
	// pages the other already cached.
	bytesRead := func(path string, op func(*V6Paths)) int64 {
		rc, err := sqlcount.OpenReadCounted(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, rc.Close()) }()
		p := NewV6Paths(rc.DB)
		defer p.Close()
		n := rc.Reset()
		op(p)
		return n()
	}
	resolve := func(p *V6Paths) {
		_, ok, err := p.Resolve(deepPath)
		require.NoError(t, err)
		require.True(t, ok)
	}
	render := func(nid int64) func(*V6Paths) {
		return func(p *V6Paths) {
			got, ok, err := p.Path(nid)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, deepPath, got)
		}
	}
	for name, op := range map[string][2]func(*V6Paths){
		"Resolve": {resolve, resolve},
		// The nid is looked up beforehand, on another connection, so the
		// measurement is the walk up alone.
		"Path": {render(nidIn(t, small)), render(nidIn(t, large))},
	} {
		s, l := bytesRead(small, op[0]), bytesRead(large, op[1])
		require.Positive(t, s, "%s: the counter must see the reads, or the gate is vacuous", name)
		assert.LessOrEqual(t, l, 4*s, "%s read %d bytes at 10 files and %d at 2000: its cost grows with the database", name, s, l)
	}
}

// roundTrip is one round trip: the deep path to its nid and back.
func roundTrip(t *testing.T, p *V6Paths) {
	got, ok, err := p.Path(nidAt(t, p, deepPath))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deepPath, got)
}

// nidAt resolves path, which must exist.
func nidAt(t *testing.T, p *V6Paths, path string) int64 {
	nid, ok, err := p.Resolve(path)
	require.NoError(t, err)
	require.True(t, ok, path)
	return nid
}

// nidIn is deepPath's nid in the database at dbPath, read on a connection of
// its own.
func nidIn(t *testing.T, dbPath string) int64 {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	p := NewV6Paths(db)
	defer p.Close()
	return nidAt(t, p, deepPath)
}

// A round trip over a real `leyline parse` output, every node of it. Set
// MACHE_TEST_LEYLINE_V6_DB to a projection-v6 .db (leyline v0.20.x) to run it.
// The fixture's layout was read from one; this checks the addressing against
// one directly, at a size the fixture never reaches.
func TestV6Paths_RoundTripOverARealParse(t *testing.T) {
	path := os.Getenv("MACHE_TEST_LEYLINE_V6_DB")
	if path == "" {
		t.Skip("MACHE_TEST_LEYLINE_V6_DB not set")
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT nid FROM nodes`)
	require.NoError(t, err)
	var nids []int64
	for rows.Next() {
		var nid int64
		require.NoError(t, rows.Scan(&nid))
		nids = append(nids, nid)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, nids)

	p := NewV6Paths(db)
	defer p.Close()
	for _, nid := range nids {
		got, ok, err := p.Path(nid)
		require.NoError(t, err)
		require.True(t, ok, "nid %d", nid)
		back, ok, err := p.Resolve(got)
		require.NoError(t, err)
		require.True(t, ok, "Resolve(%q) for nid %d", got, nid)
		require.Equal(t, nid, back, "Resolve(Path(%d)) = Resolve(%q)", nid, got)
	}
	t.Logf("round-tripped %d nodes", len(nids))
}

// V6FileNids selects exactly one file's nodes, by the file's _source id: not
// the file before it, not the file after it.
func TestV6FileNids_SelectsOneFile(t *testing.T) {
	f := v6PathsSpec(t)
	want := allNodes(t, f.DB())
	for _, src := range []string{"main.go", "pkg/a.go", "pkg/sub/b.go"} {
		rows, err := f.DB().Query(`SELECT nid FROM nodes WHERE `+V6FileNids("nid")+` ORDER BY nid`, src)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var nid int64
			require.NoError(t, rows.Scan(&nid))
			got = append(got, want[nid])
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.NotEmpty(t, got, src)
		assert.Equal(t, src, got[0], "the file node comes first")
		for _, p := range got {
			assert.True(t, p == src || strings.HasPrefix(p, src+"/"), "%s is not in %s", p, src)
		}
	}
}

// A renderer gives every node the path Path gives it, and pays for each
// ancestor once: rendering a node whose parent it already rendered costs one
// statement, however deep the node is.
func TestPathRenderer_AgreesWithPathAndRendersEachAncestorOnce(t *testing.T) {
	f := v6PathsSpec(t)
	want := allNodes(t, f.DB())
	p := NewV6Paths(f.DB())
	defer p.Close()
	r := p.Renderer()
	for nid, path := range want {
		got, ok, err := r.Path(nid)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, path, got, "nid %d", nid)
	}
	_, ok, err := r.Path(1 << 40)
	require.NoError(t, err)
	assert.False(t, ok, "a nid with no row has no path")

	sqlcount.RegisterDriver()
	db, err := sql.Open(sqlcount.DriverName, f.DBPath()+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	cp := NewV6Paths(db)
	defer cp.Close()
	cr := cp.Renderer()
	deep := nidAt(t, cp, "pkg/a.go/function_declaration_1/block")
	parent := nidAt(t, cp, "pkg/a.go/function_declaration_1")
	_, _, err = cr.Path(parent) // renders the parent and every ancestor above it
	require.NoError(t, err)
	count := sqlcount.Reset()
	got, ok, err := cr.Path(deep)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "pkg/a.go/function_declaration_1/block", got)
	assert.Equal(t, int64(1), count(), "only the node itself is new; its ancestors are remembered")
}

// A renderer over a Querier (the smell engine holds one, not the database)
// gives the same paths as one over the compiled statement.
func TestQuerierRenderer_AgreesWithPath(t *testing.T) {
	f := v6PathsSpec(t)
	want := allNodes(t, f.DB())
	r := QuerierRenderer(DB(f.DB()))
	for nid, path := range want {
		got, ok, err := r.Path(nid)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, path, got, "nid %d", nid)
	}
	_, ok, err := r.Path(1 << 40)
	require.NoError(t, err)
	assert.False(t, ok)
}

// ParsedSourceLike answers "was this language parsed at all?" on either
// projection: through _ast.source_id on v4, and by each matching file's nid
// range on v6.
func TestParsedSourceLike_BothProjections(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		f := paritySpec(t, p)
		got, err := ParsedSourceLike(DB(f.DB()), "%.go")
		require.NoError(t, err)
		assert.True(t, got, "%v: the spec parses Go files", p)
		got, err = ParsedSourceLike(DB(f.DB()), "%.rs")
		require.NoError(t, err)
		assert.False(t, got, "%v: and no Rust", p)
	}
	_, standalone := fixturedb.New(t, fixturedb.Standalone).Construct("a.go/F").Build()
	got, err := ParsedSourceLike(DB(standalone.DB()), "%.go")
	require.NoError(t, err)
	assert.False(t, got, "a projection with no _ast parsed nothing")
}

// IntValue reads an integer back from a driver (int64) or from the daemon's
// query op, which sends every value as text, and refuses text that is not
// exactly a decimal integer rather than reading some other node's nid out of it.
func TestIntValue(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int64
		ok   bool
	}{
		{int64(-7), -7, true},
		{"50331657", 3<<24 | 9, true},
		{"-1", -1, true},
		{"1.5", 0, false},
		{"12abc", 0, false},
		{"", 0, false},
		{1.0, 0, false},
		{nil, 0, false},
	} {
		got, err := IntValue(tc.in)
		if tc.ok {
			require.NoError(t, err, "%v", tc.in)
			assert.Equal(t, tc.want, got, "%v", tc.in)
		} else {
			assert.Error(t, err, "%v", tc.in)
		}
	}
}
