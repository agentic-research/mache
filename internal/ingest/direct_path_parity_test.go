package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// engineGraph is the corpus at dataPath projected the way `mache --out` does
// it: every node a row.
func engineGraph(t *testing.T, schema *api.Topology, dataPath string) *graph.SQLiteGraph {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engine.db")
	w, err := NewSQLiteWriter(p)
	require.NoError(t, err)
	require.NoError(t, NewEngine(schema, w).Ingest(dataPath))
	require.NoError(t, w.Close())
	g, err := graph.OpenSQLiteGraph(p, schema, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// directGraph is the same corpus served straight from its records: one row
// each in results(id, record), nodes projected on demand.
func directGraph(t *testing.T, schema *api.Topology, dataPath string) *graph.SQLiteGraph {
	t.Helper()
	raw, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	var records []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records))

	p := filepath.Join(t.TempDir(), "records.db")
	db, err := sql.Open("sqlite", p)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)`)
	require.NoError(t, err)
	for i, r := range records {
		_, err = db.Exec(`INSERT INTO results VALUES (?, ?)`, strconv.Itoa(i), string(r))
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	g, err := graph.OpenSQLiteGraph(p, schema, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	require.NoError(t, g.EagerScan())
	return g
}

// treeOf is every node id under root, sorted.
func treeOf(t *testing.T, g graph.Graph, root string) []string {
	t.Helper()
	var out []string
	var walk func(string)
	walk = func(id string) {
		kids, err := g.ListChildren(id)
		require.NoError(t, err)
		for _, k := range kids {
			out = append(out, k)
			if n, _ := g.GetNode(k); n != nil && n.Mode.IsDir() {
				walk(k)
			}
		}
	}
	walk(root)
	slices.Sort(out)
	return out
}

func nodeIDsSorted(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// A data source served straight from its records answers what the
// materialised projection answers: the same tree, the same file contents, the
// same definitions and the same callers. The direct path ignored `refs`
// until mache-1bf0d8, so find_callers returned nothing there.
func TestDirectPath_AnswersWhatTheEnginePathAnswers(t *testing.T) {
	schema := sharedParentSchema()
	data := withoutLastRecord(t, sharedParentData(t, 300, 3, 4))
	engine, direct := engineGraph(t, schema, data), directGraph(t, schema, data)

	tree := treeOf(t, engine, "by-area")
	require.NotEmpty(t, tree)
	require.Equal(t, tree, treeOf(t, direct, "by-area"), "the same nodes")

	buf := make([]byte, 1<<12)
	read := func(g graph.Graph, id string) string {
		n, err := g.ReadContent(id, buf, 0)
		require.NoError(t, err, id)
		return string(buf[:n])
	}
	modules, withCallers := 0, 0
	for _, id := range tree {
		node, err := engine.GetNode(id)
		require.NoError(t, err)
		if !node.Mode.IsDir() {
			assert.Equal(t, read(engine, id), read(direct, id), "content of %s", id)
			continue
		}
		name := filepath.Base(id)
		if len(name) < 3 || name[:3] != "Mod" {
			continue
		}
		modules++
		assert.Equal(t, graph.DefTokensFor(engine, id), graph.DefTokensFor(direct, id), "tokens of %s", id)
		assert.Equal(t, engine.LookupDef(name), direct.LookupDef(name), "definition of %s", name)

		want, err := engine.GetCallers(name)
		require.NoError(t, err)
		got, err := direct.GetCallers(name)
		require.NoError(t, err)
		assert.Equal(t, nodeIDsSorted(want), nodeIDsSorted(got), "callers of %s", name)
		if len(want) > 0 {
			withCallers++
		}

		viaNode, err := graph.CallersOf(direct, id, nil)
		require.NoError(t, err)
		assert.Equal(t, nodeIDsSorted(want), nodeIDsSorted(viaNode), "callers of the node %s", id)
	}
	assert.Equal(t, 300, modules, "every module directory was compared")
	assert.Greater(t, withCallers, 100, "the corpus must have callers to compare")
}

// withoutLastRecord is the corpus at dataPath minus its final record, which
// sharedParentData adds as a second record for an existing directory.
func withoutLastRecord(t *testing.T, dataPath string) string {
	t.Helper()
	raw, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	var records []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records))
	out, err := json.Marshal(records[:len(records)-1])
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "unique.json")
	require.NoError(t, os.WriteFile(p, out, 0o644))
	return p
}

// KNOWN GAP, mache-299113: two records that render the same directory. The
// engine keeps both, the second as `Mod0.2`; the direct path merges them into
// one directory, so one record's content is never served. That disagreement
// predates the refs work and is the first entry in the list ADR-0028 asks
// for. This pins it, so the day the direct path keeps both this test fails
// and the colliding record goes back into the parity corpus above.
func TestDirectPath_KnownGap_SameNameRecordsAreMerged(t *testing.T) {
	schema := sharedParentSchema()
	data := sharedParentData(t, 20, 3, 4)
	engine, direct := engineGraph(t, schema, data), directGraph(t, schema, data)

	engineKids, err := engine.ListChildren("by-area/area0/sub0")
	require.NoError(t, err)
	require.Contains(t, engineKids, "by-area/area0/sub0/Mod0.2", "the engine keeps the second record")

	directKids, err := direct.ListChildren("by-area/area0/sub0")
	require.NoError(t, err)
	assert.NotContains(t, directKids, "by-area/area0/sub0/Mod0.2",
		"the direct path now keeps same-named records apart: mache-299113 is fixed; add the collision to the parity corpus")
}

// A record with no value for a field its refs template ranges over still
// projects: a missing ref field is no references, not a dropped record.
func TestDirectPath_ARecordWithoutItsRefFieldIsKept(t *testing.T) {
	schema := sharedParentSchema()
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, []byte(`[
		{"area":"a","sub":"s","module":"HasImports","doc":"d","kind":"module","imports":["NoImports"]},
		{"area":"a","sub":"s","module":"NoImports","doc":"d","kind":"module"}
	]`), 0o644))
	direct := directGraph(t, schema, p)

	kids, err := direct.ListChildren("by-area/a/s")
	require.NoError(t, err)
	assert.Equal(t, []string{"by-area/a/s/HasImports", "by-area/a/s/NoImports"}, kids)
	callers, err := direct.GetCallers("NoImports")
	require.NoError(t, err)
	assert.Equal(t, []string{"by-area/a/s/HasImports"}, nodeIDsSorted(callers))
}

// A dotted name is defined under its last segment too, on both paths, so a
// reference to either reaches it.
func TestDirectPath_DottedNamesDefineTheirShortNameAsTheEngineDoes(t *testing.T) {
	schema := sharedParentSchema()
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, []byte(`[
		{"area":"a","sub":"s","module":"Theory.EulerSum","doc":"d","kind":"module","imports":[]},
		{"area":"a","sub":"s","module":"Theory.Uses","doc":"d","kind":"module","imports":["Theory.EulerSum"]}
	]`), 0o644))
	engine, direct := engineGraph(t, schema, p), directGraph(t, schema, p)

	const node = "by-area/a/s/Theory.EulerSum"
	want := graph.DefTokensFor(engine, node)
	require.Equal(t, []string{"EulerSum", "Theory.EulerSum"}, want, "the engine defines both names")
	assert.Equal(t, want, graph.DefTokensFor(direct, node))
	for _, token := range want {
		assert.Equal(t, engine.LookupDef(token), direct.LookupDef(token), "definition of %s", token)
	}
	callers, err := graph.CallersOf(direct, node, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"by-area/a/s/Theory.Uses"}, nodeIDsSorted(callers))
}

// A refs template over a plain string field (the common `"refs":
// ["{{.name}}"]`) and over a number reaches the same tokens on both paths:
// ref fields arrive typed, so a string is a string and not unparsed JSON.
func TestDirectPath_ScalarRefFieldsMatchTheEngine(t *testing.T) {
	schema := &api.Topology{Version: "v1", Nodes: []api.Node{{
		Name: "tools", Selector: "$",
		Children: []api.Node{{
			Name: "{{.name}}", Selector: "$[*]",
			Refs:  []string{"{{.uses}}", "v{{.version}}"},
			Files: []api.Leaf{{Name: "name", ContentTemplate: "{{.name}}"}},
		}},
	}}}
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, []byte(`[
		{"name":"alpha","uses":"beta","version":2},
		{"name":"beta","uses":"alpha","version":2},
		{"name":"gamma","uses":"alpha","version":10}
	]`), 0o644))
	engine, direct := engineGraph(t, schema, p), directGraph(t, schema, p)
	for token, want := range map[string][]string{
		"alpha": {"tools/beta", "tools/gamma"},
		"beta":  {"tools/alpha"},
		"v2":    {"tools/alpha", "tools/beta"},
		"v10":   {"tools/gamma"},
	} {
		fromEngine, err := engine.GetCallers(token)
		require.NoError(t, err)
		require.Equal(t, want, nodeIDsSorted(fromEngine), "engine: callers of %s", token)
		fromDirect, err := direct.GetCallers(token)
		require.NoError(t, err)
		assert.Equal(t, want, nodeIDsSorted(fromDirect), "direct: callers of %s", token)
	}
}
