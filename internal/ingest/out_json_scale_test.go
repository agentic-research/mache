package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
)

// The shape that made `mache --out` quadratic: many records under one shared
// directory (by-area/<area>/<sub>/<module>), each with files and refs.
func sharedParentSchema() *api.Topology {
	return &api.Topology{
		Version: "v1",
		Nodes: []api.Node{{
			Name:     "by-area",
			Selector: "$",
			Children: []api.Node{{
				Name:     "{{.area}}",
				Selector: "$[*]",
				Children: []api.Node{{
					Name:     "{{.sub}}",
					Selector: "$",
					Children: []api.Node{{
						Name:     "{{.module}}",
						Selector: "$",
						Refs:     []string{"{{range .imports}}{{.}}\n{{end}}"},
						Files: []api.Leaf{
							{Name: "doc", ContentTemplate: "{{.doc}}"},
							{Name: "kind", ContentTemplate: "{{.kind}}"},
						},
					}},
				}},
			}},
		}},
	}
}

// sharedParentData writes n module records spread over areas*subs shared
// directories and returns the file's path. Module names repeat across subs,
// so two records also merge into one existing module directory.
func sharedParentData(t *testing.T, n, areas, subs int) string {
	t.Helper()
	type rec struct {
		Area    string   `json:"area"`
		Sub     string   `json:"sub"`
		Module  string   `json:"module"`
		Doc     string   `json:"doc"`
		Kind    string   `json:"kind"`
		Imports []string `json:"imports"`
	}
	recs := make([]rec, 0, n+1)
	for i := range n {
		r := rec{
			Area:   fmt.Sprintf("area%d", i%areas),
			Sub:    fmt.Sprintf("sub%d", (i/areas)%subs),
			Module: fmt.Sprintf("Mod%d", i),
			Doc:    fmt.Sprintf("doc of %d", i),
			Kind:   "module",
		}
		if i > 0 {
			r.Imports = []string{fmt.Sprintf("Mod%d", i-1), fmt.Sprintf("Mod%d", i/2)}
		}
		recs = append(recs, r)
	}
	// A second record for an existing module directory: the merge the
	// children read exists for.
	recs = append(recs, rec{Area: "area0", Sub: "sub0", Module: "Mod0", Doc: "again", Kind: "module", Imports: []string{"Mod1"}})
	data, err := json.Marshal(recs)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, data, 0o644))
	return p
}

// countingTarget counts the child rows an ingest reads back from its store.
// Rows, not time: a count is the same on every machine.
type countingTarget struct {
	*SQLiteWriter
	rows int
}

func (c *countingTarget) ListChildren(id string) ([]string, error) {
	kids, err := c.SQLiteWriter.ListChildren(id)
	c.rows += len(kids)
	return kids, err
}

// rowsReadIngesting ingests n shared-parent records into a fresh SQLite index
// and returns how many child rows the engine read back while doing it.
func rowsReadIngesting(t *testing.T, n int) int {
	t.Helper()
	w, err := NewSQLiteWriter(filepath.Join(t.TempDir(), "out.db"))
	require.NoError(t, err)
	target := &countingTarget{SQLiteWriter: w}
	require.NoError(t, NewEngine(sharedParentSchema(), target).Ingest(sharedParentData(t, n, 2, 2)))
	require.NoError(t, w.Close())
	return target.rows
}

// `mache --out` on JSON must read back a bounded number of rows per record.
// It re-listed each shared directory's children for every record under it:
// 16,000 records took 295s and the 164,464-record Lean graph an estimated
// 28 hours, against 4 minutes in memory (mache-35e730). Doubling the records
// must not much more than double the rows read.
func TestOutJSON_RowsReadGrowLinearlyWithRecords(t *testing.T) {
	const n = 1500
	small, large := rowsReadIngesting(t, n), rowsReadIngesting(t, 2*n)
	t.Logf("child rows read back: %d records -> %d, %d records -> %d", n, small, 2*n, large)
	assert.LessOrEqual(t, large, 4*2*n, "more than 4 child rows read back per record")
	assert.LessOrEqual(t, large, 3*max(small, 1), "rows read grew faster than the record count")
}

// tableDump is every row of a table, each rendered to one string, sorted.
func tableDump(t *testing.T, dbPath, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, strings.Join(parts, "\x1f"))
	}
	require.NoError(t, rows.Err())
	slices.Sort(out)
	return out
}

// readsChildrenAnyway is a SQLiteWriter that does not say it ignores
// Node.Children, so the engine does its merge reads as it did before: the
// old behaviour, kept reachable so the new one can be compared with it.
type readsChildrenAnyway struct{ IngestionTarget }

// Skipping the merge reads must change nothing that is written. The same
// corpus, ingested with and without them, gives the same nodes, refs and
// defs, row for row. mtime is excluded: it is the time of the write.
func TestOutJSON_SkippingTheMergeReadWritesTheSameDatabase(t *testing.T) {
	data := sharedParentData(t, 400, 3, 4)
	ingest := func(wrap func(*SQLiteWriter) IngestionTarget) string {
		p := filepath.Join(t.TempDir(), "out.db")
		w, err := NewSQLiteWriter(p)
		require.NoError(t, err)
		require.NoError(t, NewEngine(sharedParentSchema(), wrap(w)).Ingest(data))
		require.NoError(t, w.Close())
		return p
	}
	fast := ingest(func(w *SQLiteWriter) IngestionTarget { return w })
	slow := ingest(func(w *SQLiteWriter) IngestionTarget { return readsChildrenAnyway{w} })

	for name, q := range map[string]string{
		"nodes":     `SELECT id, parent_id, name, kind, size, record_id, record, source_file FROM nodes`,
		"node_refs": `SELECT token, node_id FROM node_refs`,
		"node_defs": `SELECT token, node_id FROM node_defs`,
	} {
		want, got := tableDump(t, slow, q), tableDump(t, fast, q)
		require.NotEmpty(t, want, "%s: the corpus must write rows to compare", name)
		assert.Equal(t, want, got, name)
	}
}

// And nothing is lost against the in-memory store, which keeps children on
// its nodes: both backends hold the same node ids.
func TestOutJSON_SameNodesAsTheMemoryStore(t *testing.T) {
	data := sharedParentData(t, 300, 3, 4)

	p := filepath.Join(t.TempDir(), "out.db")
	w, err := NewSQLiteWriter(p)
	require.NoError(t, err)
	require.NoError(t, NewEngine(sharedParentSchema(), w).Ingest(data))
	require.NoError(t, w.Close())
	inDB := tableDump(t, p, `SELECT id FROM nodes WHERE id != ''`)

	store := graph.NewMemoryStore()
	require.NoError(t, NewEngine(sharedParentSchema(), store).Ingest(data))
	var inMemory []string
	var walk func(id string)
	walk = func(id string) {
		kids, err := store.ListChildren(id)
		require.NoError(t, err)
		for _, k := range kids {
			inMemory = append(inMemory, k)
			if n, _ := store.GetNode(k); n != nil && n.Mode.IsDir() {
				walk(k)
			}
		}
	}
	walk("")
	slices.Sort(inMemory)
	assert.Equal(t, inMemory, inDB)
}
