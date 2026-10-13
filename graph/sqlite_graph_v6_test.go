package graph

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// SQLiteGraph's reference and definition lookups read node_defs, node_refs
// and _ast, whose node columns projection-v6 renamed (mache-90cb9f). One spec
// is built as both shapes, and every lookup must answer the same.

const (
	gMain     = "main.go/function_declaration"
	gMainCall = gMain + "/block/call_expression"
	gGreet    = "pkg/a.go/function_declaration_0"
	gGreetFn  = gGreet + "/block/call_expression"
	gHelper   = "pkg/a.go/function_declaration_1"
	gClass    = "pkg/b.py/class_definition"
	gMethod   = gClass + "/block/function_definition"
	// gVar comes first in its file but last by path, so a lookup that
	// returned definitions in nid order would order them differently from v4.
	gVar = "pkg/a.go/var_declaration"
	// gDoc is a node with no _ast row of its own, under one that has one.
	gDoc = gGreet + "/doc"
)

func lookupGraph(t *testing.T, p fixturedb.Producer) *SQLiteGraph {
	b := fixturedb.New(t, p)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.Source("pkg/b.py", "python", "")
	b.Import("pkg", "example.com/m/pkg", "main.go")
	b.ASTNode(gMain, "function_declaration", "main.go",
		fixturedb.Span{StartByte: 42, EndByte: 82, StartRow: 4, StartCol: 0, EndRow: 6, EndCol: 1})
	b.ASTNode(gMain+"/block", "block", "main.go", fixturedb.Bytes(55, 82))
	b.ASTNode(gMainCall, "call_expression", "main.go",
		fixturedb.Span{StartByte: 60, EndByte: 71, StartRow: 5, StartCol: 1, EndRow: 5, EndCol: 12})
	b.ASTNode(gGreet, "function_declaration", "pkg/a.go", fixturedb.Bytes(40, 110))
	b.ASTNode(gGreet+"/block", "block", "pkg/a.go", fixturedb.Bytes(60, 110))
	b.ASTNode(gGreetFn, "call_expression", "pkg/a.go", fixturedb.Bytes(70, 80))
	b.ASTNode(gHelper, "function_declaration", "pkg/a.go", fixturedb.Bytes(112, 142))
	b.ASTNode(gVar, "var_declaration", "pkg/a.go", fixturedb.Bytes(0, 30))
	b.Construct(gDoc)
	b.ASTNode(gClass, "class_definition", "pkg/b.py", fixturedb.Bytes(0, 60))
	b.ASTNode(gClass+"/block", "block", "pkg/b.py", fixturedb.Bytes(10, 60))
	b.ASTNode(gMethod, "function_definition", "pkg/b.py", fixturedb.Bytes(12, 60))
	b.Def("Greet", gGreet, fixturedb.Function)
	b.Def("helper", gHelper, fixturedb.Function)
	b.Def("helper", gMethod, fixturedb.Method)
	b.Def("helper", gVar, fixturedb.Variable)
	b.Ref("Greet", gMain, gMainCall, "pkg")
	b.Ref("helper", gGreet, gGreetFn, "")
	path, _ := b.Build()
	g, err := OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// lookups is everything the converted methods answer, as lines.
func lookups(t *testing.T, g *SQLiteGraph) map[string]any {
	t.Helper()
	sorted := func(m map[string][]string) map[string][]string {
		for _, v := range m {
			slices.Sort(v)
		}
		return m
	}
	rr, err := g.RefRangeOf(gMainCall)
	require.NoError(t, err)
	greet, err := g.ResolveRef(gMainCall)
	require.NoError(t, err)
	helper, err := g.ResolveRef(gGreetFn)
	require.NoError(t, err)
	require.True(t, slices.IsSorted(helper.Candidates), "candidates are sorted, so a report is stable: %v", helper.Candidates)
	callees, err := g.GetCallees(gGreet)
	require.NoError(t, err)
	var calleeIDs []string
	for _, n := range callees {
		calleeIDs = append(calleeIDs, n.ID)
	}
	slices.Sort(calleeIDs)
	chains := map[string]string{}
	for _, id := range []string{gMain, gMainCall, gMethod, gDoc} {
		chain, ok, err := g.ASTKindChain(id)
		require.NoError(t, err)
		chains[id] = fmt.Sprint(ok, chain)
	}
	search := func(pattern, segment string) []string {
		hits, err := g.SearchRefs(pattern, segment, 100)
		require.NoError(t, err)
		var out []string
		for _, h := range hits {
			out = append(out, h.Token+" @ "+h.ID)
		}
		slices.Sort(out)
		return out
	}
	defs := sorted(g.DefsMap())
	return map[string]any{
		"RefRangeOf":   fmt.Sprintf("%+v", *rr),
		"ResolveRef":   fmt.Sprintf("%+v | %+v", *greet, *helper),
		"LookupDef":    g.LookupDef("helper"),
		"SearchDefs":   sorted(g.SearchDefs("Gr%", 10)),
		"DefsMap":      defs,
		"RefsMap":      sorted(g.RefsMap()),
		"GetCallees":   calleeIDs,
		"ASTKindChain": chains,
		"SearchRefs":   search("%", ""),
		"SearchRefsIn": search("%", "function_declaration_0"),
	}
}

func TestSQLiteGraphLookups_SameAnswersFromProjectionV4AndV6(t *testing.T) {
	v4g, v6g := lookupGraph(t, fixturedb.Leyline), lookupGraph(t, fixturedb.LeylineV6)
	require.Nil(t, v4g.shape())
	require.NotNil(t, v6g.shape())
	v4, v6 := lookups(t, v4g), lookups(t, v6g)

	// The v4 side must say something real, or the comparison is vacuous.
	require.Len(t, v4["DefsMap"].(map[string][]string)["helper"], 3)
	require.Contains(t, v4["ResolveRef"], "ambiguous", "helper has three definitions")
	require.Equal(t, "false []", v4["ASTKindChain"].(map[string]string)[gDoc],
		"a node with no _ast row of its own has no kind, whatever its ancestors have")
	require.NotEmpty(t, v4["RefsMap"])
	require.NotEmpty(t, v4["GetCallees"])
	require.Len(t, v4["SearchRefs"], 2)
	require.Equal(t, []string{"helper @ " + gGreetFn}, v4["SearchRefsIn"], "the segment narrows to refs under it")
	require.Contains(t, v4["ResolveRef"], "example.com/m/pkg")
	require.Contains(t, v4["ASTKindChain"].(map[string]string)[gMethod], "class_definition")

	for k := range v4 {
		assert.Equal(t, v4[k], v6[k], k)
	}
}

func TestSQLiteGraphLookups_V6Misses(t *testing.T) {
	g := lookupGraph(t, fixturedb.LeylineV6)

	rr, err := g.RefRangeOf("pkg/a.go/nope")
	require.NoError(t, err)
	assert.Nil(t, rr)

	_, err = g.ResolveRef("pkg/a.go/nope")
	assert.Error(t, err, "no reference at a node that does not exist")

	chain, ok, err := g.ASTKindChain("pkg/a.go/nope")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, chain)

	assert.Empty(t, g.LookupDef("nobody"))
}

// Over a real `leyline parse` output: every node_refs and node_defs row comes
// back as a node a consumer can open, through the bulk renderer. Set
// MACHE_TEST_LEYLINE_V6_DB to a projection-v6 .db to run it.
func TestSQLiteGraphLookups_RealProjectionV6(t *testing.T) {
	path := os.Getenv("MACHE_TEST_LEYLINE_V6_DB")
	if path == "" {
		t.Skip("MACHE_TEST_LEYLINE_V6_DB not set")
	}
	g, err := OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()
	require.NotNil(t, g.shape())

	// A v0.20.3 parse writes rows for languages injected into string literals
	// and markdown (SQL inside Go, code spans), whose nodes it does not store
	// (ley-line-open-260fcf). They are named as LLO's wire names them, so
	// every row comes back; only a stored node can be opened.
	count := func(table string) (all, dangling int) {
		require.NoError(t, g.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&all))
		require.NoError(t, g.db.QueryRow(`SELECT count(*) FROM `+table+` r
			WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.nid = r.nid)`).Scan(&dangling))
		t.Logf("%s: %d rows, %d naming no stored node", table, all, dangling)
		return all, dangling
	}
	total := func(m map[string][]string) int {
		n := 0
		for _, ids := range m {
			n += len(ids)
		}
		return n
	}
	start := time.Now()
	refs := g.refsMapFromNodesTable() // RefsMap minus the sentinel filter, which v6 rows never match
	t.Logf("rendered %d reference sites in %s", total(refs), time.Since(start))
	refRows, _ := count("node_refs")
	assert.Equal(t, refRows, total(refs))

	defs := g.DefsMap()
	defRows, danglingDefs := count("node_defs")
	assert.Equal(t, defRows, total(defs))
	unopenable := 0
	for _, ids := range defs {
		for _, id := range ids {
			n, err := g.GetNode(id)
			if err != nil {
				unopenable++
				continue
			}
			require.NotNil(t, n.Origin, "%s: a definition is located", id)
		}
	}
	assert.Equal(t, danglingDefs, unopenable, "only the injected definitions are not nodes")
}

// An occurrence in an injected language (a markdown code span) is a reference
// leyline indexes but stores no node for (ley-line-open-260fcf). v4 named it
// by a path of its own and every lookup returned it; v6 has only a nid, and
// lookups used to drop it, so markdown code spans vanished from search,
// callers and communities. It is now named as LLO's wire names it, and found
// by every lookup on both projections, each under its own projection's name.
func TestSQLiteGraph_InjectedOccurrencesAreFoundOnBothProjections(t *testing.T) {
	open := func(p fixturedb.Producer) *SQLiteGraph {
		b := fixturedb.New(t, p)
		b.Source("doc.md", "markdown", "")
		b.ASTNode("doc.md", "document", "doc.md", fixturedb.Bytes(0, 100))
		b.ASTNode("doc.md/section", "section", "doc.md", fixturedb.Bytes(0, 100))
		b.InjectedRef("Bead", "doc.md/section", "code_span")
		path, _ := b.Build()
		g, err := OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close() })
		return g
	}
	want := map[fixturedb.Producer]string{
		fixturedb.Leyline:   "doc.md/section#inj0/code_span",
		fixturedb.LeylineV6: "doc.md#inj@2",
	}
	for p, id := range want {
		g := open(p)
		assert.Equal(t, []string{id}, g.RefsMap()["Bead"], "%v: RefsMap", p)
		hits, err := g.SearchRefs("Bead", "", 10)
		require.NoError(t, err)
		require.Len(t, hits, 1, "%v: SearchRefs", p)
		assert.Equal(t, id, hits[0].ID, "%v", p)
		callers, err := g.GetCallers("Bead")
		require.NoError(t, err)
		require.Len(t, callers, 1, "%v: GetCallers", p)
		assert.Equal(t, id, callers[0].ID, "%v", p)
	}
}
