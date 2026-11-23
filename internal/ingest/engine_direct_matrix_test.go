package ingest

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
)

// updateMatrix rewrites testdata/engine_direct_matrix.md from the current
// behaviour of both paths.
var updateMatrix = flag.Bool("update-matrix", false, "rewrite testdata/engine_direct_matrix.md")

// The engine-versus-direct disagreement matrix: ADR-0028's first step for
// data sources. The same schema and records are projected two ways, the
// engine path (`mache --out`: every node a row) and the direct path (one row
// per record, projected on demand), and what each answers is compared.
//
// It changes no behaviour. It turns "the two paths probably handle things
// differently" into a committed list: each case says whether the paths agree
// on the tree, on file contents, on definitions and on callers, and what the
// first difference is. Fixing a disagreement, or introducing one, changes
// the file, so either is a reviewed diff. A case is one way a schema or a
// record can be shaped, planted on purpose.
type matrixCase struct {
	name    string
	about   string
	schema  *api.Topology
	records string
	// tokens are asked for callers in addition to every token either path
	// defines.
	tokens []string
}

func leaf(name, tmpl string) api.Leaf { return api.Leaf{Name: name, ContentTemplate: tmpl} }

// moduleSchema is the common shape: root / {{.area}} / {{.name}} with files
// and a refs template over a list.
func moduleSchema(nameTmpl string, files ...api.Leaf) *api.Topology {
	if len(files) == 0 {
		files = []api.Leaf{leaf("doc", "{{.doc}}")}
	}
	return &api.Topology{Version: "v1", Nodes: []api.Node{{
		Name: "mods", Selector: "$",
		Children: []api.Node{{
			Name: "{{.area}}", Selector: "$[*]",
			Children: []api.Node{{
				Name: nameTmpl, Selector: "$",
				Refs:  []string{"{{range .uses}}{{.}}\n{{end}}"},
				Files: files,
			}},
		}},
	}}}
}

var matrixCases = []matrixCase{
	{
		name:   "baseline",
		about:  "one record per array element, a list-valued ref, distinct names",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"One","doc":"d1","uses":["Two"]},
		           {"area":"a","name":"Two","doc":"d2","uses":[]},
		           {"area":"b","name":"Three","doc":"d3","uses":["One","Two"]}]`,
	},
	{
		name:   "same-name records",
		about:  "two records render the same directory (mache-299113)",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"Dup","doc":"first","uses":[]},
		           {"area":"a","name":"Dup","doc":"second","uses":["Other"]},
		           {"area":"a","name":"Other","doc":"o","uses":["Dup"]}]`,
	},
	{
		name:   "missing name field",
		about:  "a record lacks the field its directory name is made of",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"Named","doc":"n","uses":[]},
		           {"area":"a","doc":"anonymous","uses":["Named"]}]`,
	},
	{
		name:  "nested selector",
		about: "a child selects an array inside each record ($.tools[*])",
		schema: &api.Topology{Version: "v1", Nodes: []api.Node{{
			Name: "servers", Selector: "$",
			Children: []api.Node{{
				Name: "{{.name}}", Selector: "$[*]",
				Children: []api.Node{{
					Name: "{{.tool}}", Selector: "$.tools[*]",
					Files: []api.Leaf{leaf("desc", "{{.desc}}")},
				}},
			}},
		}}},
		records: `[{"name":"weather","tools":[{"tool":"forecast","desc":"f"},{"tool":"alerts","desc":"a"}]},
		           {"name":"notes","tools":[{"tool":"read","desc":"r"}]}]`,
	},
	{
		name:  "templated root",
		about: "the root node's name is a template",
		schema: &api.Topology{Version: "v1", Nodes: []api.Node{{
			Name: "{{.area}}", Selector: "$[*]",
			Files: []api.Leaf{leaf("doc", "{{.doc}}")},
		}}},
		records: `[{"area":"x","doc":"dx"},{"area":"y","doc":"dy"}]`,
	},
	{
		name:    "templated file name",
		about:   "a file's name is a template",
		schema:  moduleSchema("{{.name}}", leaf("{{.kind}}.txt", "{{.doc}}")),
		records: `[{"area":"a","name":"One","kind":"note","doc":"d1","uses":[]}]`,
	},
	{
		name:   "empty content",
		about:  "a file whose template renders empty for one record",
		schema: moduleSchema("{{.name}}", leaf("doc", "{{.doc}}"), leaf("extra", "{{.extra}}")),
		records: `[{"area":"a","name":"Full","doc":"d","extra":"e","uses":[]},
		           {"area":"a","name":"Bare","doc":"d","extra":"","uses":[]}]`,
	},
	{
		name:   "absent content field",
		about:  "a file's template names a field one record does not have",
		schema: moduleSchema("{{.name}}", leaf("doc", "{{.doc}}"), leaf("owner", "{{.owner}}")),
		records: `[{"area":"a","name":"Owned","doc":"d","owner":"me","uses":[]},
		           {"area":"a","name":"Orphan","doc":"d","uses":[]}]`,
	},
	{
		name:   "dotted names",
		about:  "a name with dots is also defined under its last segment",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"Theory.EulerSum","doc":"d","uses":[]},
		           {"area":"a","name":"Theory.Uses","doc":"d","uses":["EulerSum","Theory.EulerSum"]}]`,
	},
	{
		name:   "slash in a name",
		about:  "a rendered name contains / and %",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"io/fs","doc":"d","uses":[]},
		           {"area":"a","name":"50%","doc":"d","uses":["io/fs"]}]`,
		tokens: []string{"io/fs", "50%"},
	},
	{
		name:  "scalar and numeric refs",
		about: "refs over a string field and a number",
		schema: &api.Topology{Version: "v1", Nodes: []api.Node{{
			Name: "tools", Selector: "$",
			Children: []api.Node{{
				Name: "{{.name}}", Selector: "$[*]",
				Refs:  []string{"{{.uses}}", "v{{.version}}"},
				Files: []api.Leaf{leaf("name", "{{.name}}")},
			}},
		}}},
		records: `[{"name":"alpha","uses":"beta","version":2},{"name":"beta","uses":"alpha","version":10}]`,
		tokens:  []string{"v2", "v10"},
	},
	{
		name:   "missing ref field",
		about:  "a record has no value for the field its refs range over",
		schema: moduleSchema("{{.name}}"),
		records: `[{"area":"a","name":"Uses","doc":"d","uses":["Plain"]},
		           {"area":"a","name":"Plain","doc":"d"}]`,
	},
}

// observation is everything one path answers about a corpus.
type observation struct {
	err     string
	tree    []string
	content map[string]string
	defs    map[string][]string // directory -> tokens it defines
	callers map[string][]string // token -> caller ids
}

func observe(t *testing.T, g *graph.SQLiteGraph, roots, tokens []string) observation {
	t.Helper()
	o := observation{content: map[string]string{}, defs: map[string][]string{}, callers: map[string][]string{}}
	buf := make([]byte, 1<<14)
	for _, root := range roots {
		if _, err := g.GetNode(root); err != nil {
			continue
		}
		var walk func(string)
		walk = func(id string) {
			kids, err := g.ListChildren(id)
			if err != nil {
				return
			}
			for _, k := range kids {
				o.tree = append(o.tree, k)
				n, _ := g.GetNode(k)
				if n == nil {
					continue
				}
				if !n.Mode.IsDir() {
					m, err := g.ReadContent(k, buf, 0)
					if err != nil {
						o.content[k] = "ERR " + err.Error()
					} else {
						o.content[k] = string(buf[:m])
					}
					continue
				}
				if toks, err := g.DefTokensOf(k); err == nil && len(toks) > 0 {
					o.defs[k] = toks
				}
				walk(k)
			}
		}
		o.tree = append(o.tree, root)
		walk(root)
	}
	slices.Sort(o.tree)
	for _, tok := range tokens {
		cs, err := g.GetCallers(tok)
		if err != nil {
			o.callers[tok] = []string{"ERR " + err.Error()}
			continue
		}
		if ids := nodeIDsSorted(cs); len(ids) > 0 {
			o.callers[tok] = ids
		}
	}
	return o
}

// rootsOf is the root names a schema can be looked up by: static ones by
// name, and for a templated root, the names the records render.
func rootsOf(c matrixCase, recs []map[string]any) []string {
	var roots []string
	for _, n := range c.schema.Nodes {
		if !strings.Contains(n.Name, "{{") {
			roots = append(roots, n.Name)
			continue
		}
		field := strings.TrimSuffix(strings.TrimPrefix(n.Name, "{{."), "}}")
		for _, r := range recs {
			if v, ok := r[field].(string); ok {
				roots = append(roots, v)
			}
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// tokensOf is every token either path defines, plus the case's own.
func tokensOf(c matrixCase, obs ...observation) []string {
	set := map[string]bool{}
	for _, t := range c.tokens {
		set[t] = true
	}
	for _, o := range obs {
		for _, toks := range o.defs {
			for _, t := range toks {
				set[t] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// compare says whether two answers agree, and if not, the first difference.
func compare[V any](a, b map[string]V, eq func(V, V) bool, show func(V) string) string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var diffs []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		va, inA := a[k]
		vb, inB := b[k]
		switch {
		case !inA:
			diffs = append(diffs, fmt.Sprintf("`%s` only on direct (%s)", k, show(vb)))
		case !inB:
			diffs = append(diffs, fmt.Sprintf("`%s` only on engine (%s)", k, show(va)))
		case !eq(va, vb):
			diffs = append(diffs, fmt.Sprintf("`%s`: engine %s, direct %s", k, show(va), show(vb)))
		}
	}
	switch len(diffs) {
	case 0:
		return "agree"
	case 1:
		return "**differ**: " + diffs[0]
	default:
		return fmt.Sprintf("**differ** (%d): %s; …", len(diffs), diffs[0])
	}
}

func asSet(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return fmt.Sprintf("%q", s)
}

func joinIDs(ids []string) string { return "[" + strings.Join(ids, ", ") + "]" }

// matrixRow compares the two paths on one case.
func matrixRow(t *testing.T, c matrixCase) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, []byte(c.records), 0o644))
	var recs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.records), &recs), c.name)
	roots := rootsOf(c, recs)

	engine := observe(t, engineGraph(t, c.schema, p), roots, c.tokens)
	direct := observe(t, directGraph(t, c.schema, p), roots, c.tokens)
	tokens := tokensOf(c, engine, direct)
	engine = observe(t, engineGraph(t, c.schema, p), roots, tokens)
	direct = observe(t, directGraph(t, c.schema, p), roots, tokens)

	sameStrings := func(a, b []string) bool { return slices.Equal(a, b) }
	return []string{
		c.name,
		compare(asSet(engine.tree), asSet(direct.tree), func(a, b bool) bool { return a == b }, func(bool) string { return "node" }),
		compare(engine.content, direct.content, func(a, b string) bool { return a == b }, quote),
		compare(engine.defs, direct.defs, sameStrings, joinIDs),
		compare(engine.callers, direct.callers, sameStrings, joinIDs),
	}
}

func renderMatrix(rows [][]string) string {
	var b strings.Builder
	b.WriteString(`# Engine vs direct: what each data path answers

Generated by ` + "`go test ./internal/ingest/ -run TestEngineDirectMatrix -update-matrix`" + `.
Do not edit by hand.

Each case projects one planted schema and set of records two ways: the
**engine** path (` + "`mache --out`" + `, every node a row) and the **direct** path (one
row per record, nodes projected on demand). A cell says whether the two
answer the same, and if not, the first difference. This is ADR-0028's first
step for data sources: the list of disagreements, before deciding which
answer is specified. See ` + "`engine_direct_matrix_test.go`" + ` for each case.

| Case | About | Tree | Content | Definitions | Callers |
| --- | --- | --- | --- | --- | --- |
`)
	for i, r := range rows {
		esc := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			esc(r[0]), esc(matrixCases[i].about), esc(r[1]), esc(r[2]), esc(r[3]), esc(r[4]))
	}
	return b.String()
}

// The committed matrix is what both paths do today. A change to either path
// that changes an answer fails here until the file is regenerated, so the
// change and its effect on agreement are reviewed together.
func TestEngineDirectMatrix(t *testing.T) {
	rows := make([][]string, 0, len(matrixCases))
	for _, c := range matrixCases {
		rows = append(rows, matrixRow(t, c))
	}
	got := renderMatrix(rows)
	golden := filepath.Join("testdata", "engine_direct_matrix.md")
	if *updateMatrix {
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "run with -update-matrix to create it")
	require.Equal(t, string(want), got,
		"the engine/direct agreement changed: review the difference, then run with -update-matrix")
}
