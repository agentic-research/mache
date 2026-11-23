// Package protosmall is a throwaway measurement, not product code: how cheap
// is a data projection that stores each record once plus a refs index and
// projects nodes on demand, against the engine path that materialises every
// node as a row? Run with PROTO_N set.
package protosmall

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

type rec struct {
	Area    string   `json:"area"`
	Sub     string   `json:"sub"`
	Module  string   `json:"module"`
	Doc     string   `json:"doc"`
	Kind    string   `json:"kind"`
	Imports []string `json:"imports"`
}

func schema() *api.Topology {
	return &api.Topology{Version: "v1", Nodes: []api.Node{{
		Name: "by-area", Selector: "$",
		Children: []api.Node{{
			Name: "{{.area}}", Selector: "$[*]",
			Children: []api.Node{{
				Name: "{{.sub}}", Selector: "$",
				Children: []api.Node{{
					Name: "{{.module}}", Selector: "$",
					Refs: []string{"{{range .imports}}{{.}}\n{{end}}"},
					Files: []api.Leaf{
						{Name: "doc", ContentTemplate: "{{.doc}}"},
						{Name: "kind", ContentTemplate: "{{.kind}}"},
						{Name: "imports", ContentTemplate: "{{range .imports}}{{.}}\n{{end}}"},
						{Name: "name", ContentTemplate: "{{.module}}"},
					},
				}},
			}},
		}},
	}}}
}

func gen(n int) ([]rec, int) {
	rng := rand.New(rand.NewSource(1))
	recs := make([]rec, n)
	refs := 0
	for i := range recs {
		r := rec{
			Area: fmt.Sprintf("Area%d", i%317), Sub: fmt.Sprintf("Sub%d", (i/317)%41),
			Module: fmt.Sprintf("Mathlib.Area%d.Sub%d.Mod%d", i%317, (i/317)%41, i),
			Doc:    strings.Repeat("x", 60), Kind: "module",
		}
		if i > 10 {
			k := 1 + rng.Intn(8)
			if rng.Intn(50) == 0 {
				r.Imports = append(r.Imports, recs[7].Module) // a hub
			}
			for range k {
				r.Imports = append(r.Imports, recs[rng.Intn(i)].Module)
			}
		}
		refs += len(r.Imports)
		recs[i] = r
	}
	return recs, refs
}

func size(paths ...string) string {
	var total int64
	for _, p := range paths {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if st, err := os.Stat(p + suffix); err == nil {
				total += st.Size()
			}
		}
	}
	return fmt.Sprintf("%.0f MB", float64(total)/1e6)
}

func TestSmallForm(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("PROTO_N"))
	if n == 0 {
		t.Skip("set PROTO_N")
	}
	recs, nrefs := gen(n)
	t.Logf("records %d, refs %d", n, nrefs)
	dir := t.TempDir()
	step := func(name string, start time.Time, extra string) {
		t.Logf("%-46s %8s  %s", name, time.Since(start).Round(time.Millisecond), extra)
	}

	// ---- small form: one row per record -------------------------------------
	small := filepath.Join(dir, "small.db")
	start := time.Now()
	db, err := sql.Open("sqlite", small)
	must(t, err)
	_, err = db.Exec(`CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)`)
	must(t, err)
	tx, err := db.Begin()
	must(t, err)
	ins, err := tx.Prepare(`INSERT INTO results VALUES (?, ?)`)
	must(t, err)
	for i, r := range recs {
		b, _ := json.Marshal(r)
		_, err = ins.Exec(strconv.Itoa(i), string(b))
		must(t, err)
	}
	must(t, tx.Commit())
	must(t, db.Close())
	step("small: write records table", start, size(small))

	start = time.Now()
	sg, err := graph.OpenSQLiteGraph(small, schema(), machetmpl.Render)
	must(t, err)
	defer func() { _ = sg.Close() }()
	must(t, sg.EagerScan())
	step("small: open + scan (every start)", start, "")

	// The refs index: what a scan that honoured `refs` would add. Rendered
	// through the real template, from the stored record.
	start = time.Now()
	rdb, err := sql.Open("sqlite", small)
	must(t, err)
	rows, err := rdb.Query(`SELECT record FROM results`)
	must(t, err)
	refTmpl := schema().Nodes[0].Children[0].Children[0].Children[0].Refs[0]
	added := 0
	for rows.Next() {
		var raw string
		must(t, rows.Scan(&raw))
		var values map[string]any
		must(t, json.Unmarshal([]byte(raw), &values))
		rendered, err := machetmpl.Render(refTmpl, values)
		must(t, err)
		node := "by-area/" + values["area"].(string) + "/" + values["sub"].(string) + "/" + values["module"].(string)
		for _, tok := range strings.Split(rendered, "\n") {
			if tok = strings.TrimSpace(tok); tok != "" {
				must(t, sg.AddRef(tok, node))
				added++
			}
		}
	}
	must(t, rows.Err())
	_ = rows.Close()
	_ = rdb.Close()
	must(t, sg.FlushRefs())
	step("small: build refs index (every start, today)", start, fmt.Sprintf("%d refs, sidecar %s", added, size(small+".refs.db")))

	query(t, "small", sg, recs)

	// ---- engine form: every node a row --------------------------------------
	data := filepath.Join(dir, "data.json")
	b, _ := json.Marshal(recs)
	must(t, os.WriteFile(data, b, 0o644))
	big := filepath.Join(dir, "big.db")
	start = time.Now()
	w, err := ingest.NewSQLiteWriter(big)
	must(t, err)
	must(t, ingest.NewEngine(schema(), w).Ingest(data))
	step("engine: ingest", start, "")
	must(t, w.Close())
	step("engine: ingest + close (once)", start, size(big))

	start = time.Now()
	bg, err := graph.OpenSQLiteGraph(big, schema(), machetmpl.Render)
	must(t, err)
	defer func() { _ = bg.Close() }()
	step("engine: open (every start)", start, "")
	query(t, "engine", bg, recs)
}

func query(t *testing.T, label string, g *graph.SQLiteGraph, recs []rec) {
	t.Helper()
	sub := "by-area/" + recs[20].Area + "/" + recs[20].Sub
	start := time.Now()
	kids, err := g.ListChildren(sub)
	must(t, err)
	t.Logf("%-46s %8s  %d children", label+": list one sub directory", time.Since(start).Round(time.Microsecond), len(kids))

	node := sub + "/" + recs[20].Module
	buf := make([]byte, 4096)
	start = time.Now()
	nread, err := g.ReadContent(node+"/imports", buf, 0)
	must(t, err)
	t.Logf("%-46s %8s  %d bytes", label+": read one file", time.Since(start).Round(time.Microsecond), nread)

	start = time.Now()
	callers, err := g.GetCallers(recs[7].Module)
	must(t, err)
	t.Logf("%-46s %8s  %d callers", label+": callers of the hub", time.Since(start).Round(time.Microsecond), len(callers))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
