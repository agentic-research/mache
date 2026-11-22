package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
)

func TestRefTokens(t *testing.T) {
	assert.Nil(t, refTokens(""))
	assert.Equal(t, []string{"A"}, refTokens("A"))
	assert.Equal(t, []string{" spaced "}, refTokens(" spaced "), "a single-line ref is one token, exactly as rendered")
	assert.Equal(t, []string{"A", "B", "C"}, refTokens("A\nB\n\n  C \r\n"))
	assert.Empty(t, refTokens("\n\n"), "a range over an empty list names nothing")
}

// listRefsSchema projects modules whose `imports` is a list, with ONE refs
// template ranging over it (#820: this used to need a template per index).
func listRefsSchema() *api.Topology {
	return &api.Topology{
		Version: "v1",
		Nodes: []api.Node{{
			Name:     "modules",
			Selector: "$",
			Children: []api.Node{{
				Name:     "{{.name}}",
				Selector: "$[*]",
				Refs:     []string{"{{range .imports}}{{.}}\n{{end}}"},
				Files:    []api.Leaf{{Name: "name", ContentTemplate: "{{.name}}"}},
			}},
		}},
	}
}

const listRefsData = `[
	{"name":"A","imports":["B","C","D"]},
	{"name":"B","imports":["D"]},
	{"name":"D","imports":[]}
]`

func writeListRefsData(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(p, []byte(listRefsData), 0o644))
	return p
}

// The in-memory path, which `mache serve data.json` and a JSON mount take.
func TestListValuedRefs_MemoryStore(t *testing.T) {
	store := graph.NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, NewEngine(listRefsSchema(), store).Ingest(writeListRefsData(t)))
	require.NoError(t, store.FlushRefs())

	for token, want := range map[string][]string{
		"B": {"modules/A"},
		"C": {"modules/A"},
		"D": {"modules/A", "modules/B"},
	} {
		callers, err := store.GetCallers(token)
		require.NoError(t, err)
		var got []string
		for _, c := range callers {
			got = append(got, c.ID)
		}
		assert.ElementsMatch(t, want, got, "callers of %s", token)
	}
	joined, err := store.GetCallers("B\nC\nD\n")
	require.NoError(t, err)
	assert.Empty(t, joined, "the rendered list must not be indexed as one token")
}

// The streaming path, which `mache --out` and `mache build` take.
func TestListValuedRefs_SQLiteWriter(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.db")
	w, err := NewSQLiteWriter(out)
	require.NoError(t, err)
	require.NoError(t, NewEngine(listRefsSchema(), w).Ingest(writeListRefsData(t)))
	require.NoError(t, w.Close())

	db, err := sql.Open("sqlite", out)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT token, node_id FROM node_refs ORDER BY token, node_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got [][2]string
	for rows.Next() {
		var r [2]string
		require.NoError(t, rows.Scan(&r[0], &r[1]))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, [][2]string{
		{"B", "modules/A"}, {"C", "modules/A"}, {"D", "modules/A"}, {"D", "modules/B"},
	}, got)
}

// The .db streaming path (`mache --data x.db --out`), which renders refs in
// collectNodes rather than against the store.
func TestListValuedRefs_DBSourceStreaming(t *testing.T) {
	src := createTestDB(t, []string{
		`{"name":"A","imports":["B","C","D"]}`,
		`{"name":"B","imports":["D"]}`,
		`{"name":"D","imports":[]}`,
	})
	store := graph.NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, NewEngine(listRefsSchema(), store).Ingest(src))
	require.NoError(t, store.FlushRefs())

	callers, err := store.GetCallers("D")
	require.NoError(t, err)
	var got []string
	for _, c := range callers {
		got = append(got, c.ID)
	}
	assert.ElementsMatch(t, []string{"modules/A", "modules/B"}, got)
	callers, err = store.GetCallers("C")
	require.NoError(t, err)
	assert.Len(t, callers, 1)
}
