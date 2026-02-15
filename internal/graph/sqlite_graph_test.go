package graph

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// testRenderer is a minimal template renderer matching ingest.RenderTemplate.
var testTmplFuncs = template.FuncMap{
	"json": func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
	"first": func(v any) any {
		if s, ok := v.([]any); ok && len(s) > 0 {
			return s[0]
		}
		return nil
	},
	"slice": func(s string, start, end int) string {
		if start < 0 {
			start = 0
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			return ""
		}
		return s[start:end]
	},
}

func testRender(tmpl string, values map[string]any) (string, error) {
	t, err := template.New("").Funcs(testTmplFuncs).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func createTestDB(t *testing.T, records map[string]string) string {
	return createTestDBWithTable(t, "results", records)
}

func createTestDBWithTable(t *testing.T, tableName string, records map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(fmt.Sprintf("CREATE TABLE %s (id TEXT PRIMARY KEY, record TEXT NOT NULL)", tableName)); err != nil {
		t.Fatal(err)
	}
	for id, rec := range records {
		if _, err := db.Exec(fmt.Sprintf("INSERT INTO %s (id, record) VALUES (?, ?)", tableName), id, rec); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

func kevSchema() *api.Topology {
	return &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "vulns",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.item.cveID}}",
						Selector: "$[*]",
						Files: []api.Leaf{
							{Name: "vendor", ContentTemplate: "{{.item.vendorProject}}"},
							{Name: "product", ContentTemplate: "{{.item.product}}"},
							{Name: "description", ContentTemplate: "{{.item.shortDescription}}"},
						},
					},
				},
			},
		},
	}
}

func nvdSchema() *api.Topology {
	return &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "by-cve",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{slice .item.cve.published 0 4}}",
						Selector: "$[*]",
						Children: []api.Node{
							{
								Name:     "{{slice .item.cve.published 5 7}}",
								Selector: "$",
								Children: []api.Node{
									{
										Name:     "{{.item.cve.id}}",
										Selector: "$",
										Files: []api.Leaf{
											{Name: "description", ContentTemplate: "{{with (index .item.cve.descriptions 0)}}{{.value}}{{end}}"},
											{Name: "status", ContentTemplate: "{{.item.cve.vulnStatus}}"},
											{Name: "raw.json", ContentTemplate: "{{. | json}}"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSQLiteGraph_KEV_ListChildren(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"RCE in Widget"}}`,
		"CVE-2024-0002": `{"schema":"kev","identifier":"CVE-2024-0002","item":{"cveID":"CVE-2024-0002","vendorProject":"Globex","product":"Gizmo","shortDescription":"SQLi in Gizmo"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// Root
	roots, err := g.ListChildren("")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != "vulns" {
		t.Fatalf("roots = %v, want [vulns]", roots)
	}

	// vulns directory
	children, err := g.ListChildren("vulns")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("vulns children = %v, want 2 entries", children)
	}

	// CVE directory (files)
	files, err := g.ListChildren("vulns/CVE-2024-0001")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("CVE files = %v, want 3", files)
	}
}

func TestSQLiteGraph_KEV_GetNode(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"RCE in Widget"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// Directory nodes
	for _, path := range []string{"vulns", "vulns/CVE-2024-0001"} {
		node, err := g.GetNode(path)
		if err != nil {
			t.Fatalf("GetNode(%q) error: %v", path, err)
		}
		if !node.Mode.IsDir() {
			t.Errorf("GetNode(%q) should be directory", path)
		}
	}

	// File node
	node, err := g.GetNode("vulns/CVE-2024-0001/vendor")
	if err != nil {
		t.Fatal(err)
	}
	if node.Mode.IsDir() {
		t.Error("vendor should be a file")
	}
	if string(node.Data) != "Acme" {
		t.Errorf("vendor content = %q, want %q", node.Data, "Acme")
	}

	// Not found
	_, err = g.GetNode("vulns/CVE-9999-0001")
	if err != ErrNotFound {
		t.Errorf("GetNode(missing) err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteGraph_KEV_ReadContent(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"RCE in Widget"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	tests := []struct {
		path string
		want string
	}{
		{"vulns/CVE-2024-0001/vendor", "Acme"},
		{"vulns/CVE-2024-0001/product", "Widget"},
		{"vulns/CVE-2024-0001/description", "RCE in Widget"},
	}

	for _, tt := range tests {
		buf := make([]byte, 1024)
		n, err := g.ReadContent(tt.path, buf, 0)
		if err != nil {
			t.Fatalf("ReadContent(%q) error: %v", tt.path, err)
		}
		got := string(buf[:n])
		if got != tt.want {
			t.Errorf("ReadContent(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestSQLiteGraph_KEV_ReadContent_WithOffset(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"RCE in Widget"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	buf := make([]byte, 1024)
	n, err := g.ReadContent("vulns/CVE-2024-0001/vendor", buf, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "me" {
		t.Errorf("ReadContent with offset = %q, want %q", buf[:n], "me")
	}
}

func TestSQLiteGraph_NVD_TemporalSharding(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"nvd","identifier":"CVE-2024-0001","item":{"cve":{"id":"CVE-2024-0001","descriptions":[{"lang":"en","value":"Buffer overflow in FooBar"}],"published":"2024-01-15T00:00:00Z","vulnStatus":"Analyzed"}}}`,
		"CVE-2024-0002": `{"schema":"nvd","identifier":"CVE-2024-0002","item":{"cve":{"id":"CVE-2024-0002","descriptions":[{"lang":"en","value":"Auth bypass in BazQux"}],"published":"2024-02-01T00:00:00Z","vulnStatus":"Modified"}}}`,
		"CVE-2023-0001": `{"schema":"nvd","identifier":"CVE-2023-0001","item":{"cve":{"id":"CVE-2023-0001","descriptions":[{"lang":"en","value":"Null deref in Quux"}],"published":"2023-06-01T00:00:00Z","vulnStatus":"Analyzed"}}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, nvdSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// Root
	roots, err := g.ListChildren("")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != "by-cve" {
		t.Fatalf("roots = %v", roots)
	}

	// Year directories
	years, err := g.ListChildren("by-cve")
	if err != nil {
		t.Fatal(err)
	}
	if len(years) != 2 {
		t.Fatalf("years = %v, want 2", years)
	}

	// Months in 2024
	months2024, err := g.ListChildren("by-cve/2024")
	if err != nil {
		t.Fatal(err)
	}
	if len(months2024) != 2 {
		t.Fatalf("2024 months = %v, want 2", months2024)
	}

	// CVEs in 2024/01
	cves202401, err := g.ListChildren("by-cve/2024/01")
	if err != nil {
		t.Fatal(err)
	}
	if len(cves202401) != 1 {
		t.Fatalf("2024/01 CVEs = %v, want 1", cves202401)
	}

	// CVEs in 2024/02
	cves202402, err := g.ListChildren("by-cve/2024/02")
	if err != nil {
		t.Fatal(err)
	}
	if len(cves202402) != 1 {
		t.Fatalf("2024/02 CVEs = %v, want 1", cves202402)
	}

	// Months in 2023
	months2023, err := g.ListChildren("by-cve/2023")
	if err != nil {
		t.Fatal(err)
	}
	if len(months2023) != 1 {
		t.Fatalf("2023 months = %v, want 1", months2023)
	}

	// File content
	desc, err := g.GetNode("by-cve/2024/01/CVE-2024-0001/description")
	if err != nil {
		t.Fatal(err)
	}
	if string(desc.Data) != "Buffer overflow in FooBar" {
		t.Errorf("description = %q", desc.Data)
	}

	status, err := g.GetNode("by-cve/2024/01/CVE-2024-0001/status")
	if err != nil {
		t.Fatal(err)
	}
	if string(status.Data) != "Analyzed" {
		t.Errorf("status = %q", status.Data)
	}

	// Cross-year
	desc2023, err := g.GetNode("by-cve/2023/06/CVE-2023-0001/description")
	if err != nil {
		t.Fatal(err)
	}
	if string(desc2023.Data) != "Null deref in Quux" {
		t.Errorf("2023 description = %q", desc2023.Data)
	}
}

func TestSQLiteGraph_NVD_RawJSON(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"nvd","identifier":"CVE-2024-0001","item":{"cve":{"id":"CVE-2024-0001","descriptions":[{"lang":"en","value":"test"}],"published":"2024-01-15T00:00:00Z","vulnStatus":"Analyzed"}}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, nvdSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	node, err := g.GetNode("by-cve/2024/01/CVE-2024-0001/raw.json")
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(node.Data, &parsed); err != nil {
		t.Fatalf("raw.json is not valid JSON: %v", err)
	}
	if parsed["identifier"] != "CVE-2024-0001" {
		t.Errorf("raw.json identifier = %v", parsed["identifier"])
	}
}

func TestSQLiteGraph_LeadingSlashNormalization(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// All these should work identically
	for _, path := range []string{"/vulns", "vulns"} {
		node, err := g.GetNode(path)
		if err != nil {
			t.Fatalf("GetNode(%q) error: %v", path, err)
		}
		if !node.Mode.IsDir() {
			t.Errorf("GetNode(%q) should be dir", path)
		}
	}

	for _, path := range []string{"/", ""} {
		children, err := g.ListChildren(path)
		if err != nil {
			t.Fatalf("ListChildren(%q) error: %v", path, err)
		}
		if len(children) != 1 {
			t.Errorf("ListChildren(%q) = %v, want 1 root", path, children)
		}
	}
}

func TestSQLiteGraph_NotFound(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	notFoundPaths := []string{
		"nonexistent",
		"vulns/CVE-9999-0001",
		"vulns/CVE-2024-0001/nonexistent",
	}
	for _, path := range notFoundPaths {
		_, err := g.GetNode(path)
		if err != ErrNotFound {
			t.Errorf("GetNode(%q) err = %v, want ErrNotFound", path, err)
		}
	}
}

func TestSQLiteGraph_EmptyDB(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// Root listing works
	roots, err := g.ListChildren("")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %v", roots)
	}

	// Schema root exists but has no children
	children, err := g.ListChildren("vulns")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Errorf("vulns should have 0 children, got %v", children)
	}
}

func TestSQLiteGraph_Integration_KEV(t *testing.T) {
	kevDB := os.Getenv("MACHE_TEST_KEV_DB")
	if kevDB == "" {
		t.Skip("MACHE_TEST_KEV_DB not set")
	}
	if _, err := os.Stat(kevDB); os.IsNotExist(err) {
		t.Skip("KEV database not found at " + kevDB)
	}

	schemaJSON, err := os.ReadFile("../../examples/kev-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema api.Topology
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatal(err)
	}

	g, err := OpenSQLiteGraph(kevDB, &schema, testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	children, err := g.ListChildren("vulns")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) < 1000 {
		t.Errorf("KEV should have >1000 vulns, got %d", len(children))
	}

	// Spot-check a known CVE
	if len(children) > 0 {
		first := children[0]
		files, err := g.ListChildren(first)
		if err != nil {
			t.Fatalf("ListChildren(%q) error: %v", first, err)
		}
		if len(files) == 0 {
			t.Errorf("CVE directory should have files")
		}
		fmt.Printf("KEV: %d vulns, first=%s (%d files)\n", len(children), filepath.Base(first), len(files))
	}
}

func TestSQLiteGraph_Integration_NVD(t *testing.T) {
	nvdDB := os.Getenv("MACHE_TEST_NVD_DB")
	if nvdDB == "" {
		t.Skip("MACHE_TEST_NVD_DB not set")
	}
	if _, err := os.Stat(nvdDB); os.IsNotExist(err) {
		t.Skip("NVD database not found at " + nvdDB)
	}

	schemaJSON, err := os.ReadFile("../../examples/nvd-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema api.Topology
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatal(err)
	}

	g, err := OpenSQLiteGraph(nvdDB, &schema, testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	years, err := g.ListChildren("by-cve")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("NVD: %d year directories\n", len(years))

	if len(years) > 0 {
		// Check 2024 has month directories (not CVEs directly)
		months, err := g.ListChildren("by-cve/2024")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("NVD 2024: %d month directories\n", len(months))
		if len(months) > 12 {
			t.Errorf("2024 should have <=12 month dirs, got %d", len(months))
		}

		// Drill into first month to find CVEs
		if len(months) > 0 {
			cves, err := g.ListChildren(months[0])
			if err != nil {
				t.Fatalf("ListChildren(%q) error: %v", months[0], err)
			}
			fmt.Printf("NVD %s: %d CVEs\n", filepath.Base(months[0]), len(cves))

			// Read a file
			if len(cves) > 0 {
				descPath := cves[0] + "/description"
				buf := make([]byte, 4096)
				n, err := g.ReadContent(descPath, buf, 0)
				if err != nil {
					t.Fatalf("ReadContent(%q) error: %v", descPath, err)
				}
				fmt.Printf("NVD first CVE description: %s\n", buf[:n])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks — run with real data to validate streaming scan performance:
//   (Data generated by Venturi: https://github.com/agentic-research/venturi)
//
//   MACHE_TEST_KEV_DB=~/.agentic-research/venturi/kev/results/results.db \
//   MACHE_TEST_NVD_DB=~/.agentic-research/venturi/nvd/results/results.db \
//   go test ./internal/graph/... -bench=BenchmarkScanRoot -benchmem -count=3
// ---------------------------------------------------------------------------

func BenchmarkScanRoot_KEV(b *testing.B) {
	kevDB := os.Getenv("MACHE_TEST_KEV_DB")
	if kevDB == "" {
		b.Skip("MACHE_TEST_KEV_DB not set")
	}

	schema := kevSchema()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, err := OpenSQLiteGraph(kevDB, schema, testRender)
		if err != nil {
			b.Fatal(err)
		}
		if err := g.EagerScan(); err != nil {
			b.Fatal(err)
		}
		_ = g.Close()
	}
}

func BenchmarkScanRoot_NVD(b *testing.B) {
	nvdDB := os.Getenv("MACHE_TEST_NVD_DB")
	if nvdDB == "" {
		b.Skip("MACHE_TEST_NVD_DB not set")
	}

	schemaJSON, err := os.ReadFile("../../examples/nvd-schema.json")
	if err != nil {
		b.Fatal(err)
	}
	var schema api.Topology
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, err := OpenSQLiteGraph(nvdDB, &schema, testRender)
		if err != nil {
			b.Fatal(err)
		}
		if err := g.EagerScan(); err != nil {
			b.Fatal(err)
		}
		_ = g.Close()
	}
}

// BenchmarkScanRoot_Synthetic benchmarks scan with in-memory test data (no env vars needed).
// Useful for CI and for A/B comparison of scan implementations.
func BenchmarkScanRoot_Synthetic(b *testing.B) {
	// Build a 10K record DB
	const numRecords = 10000
	records := make(map[string]string, numRecords)
	for i := 0; i < numRecords; i++ {
		id := fmt.Sprintf("CVE-2024-%04d", i)
		records[id] = fmt.Sprintf(
			`{"schema":"kev","identifier":"%s","item":{"cveID":"%s","vendorProject":"Vendor%d","product":"Product%d","shortDescription":"Desc %d"}}`,
			id, id, i%100, i%50, i,
		)
	}

	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)"); err != nil {
		b.Fatal(err)
	}
	tx, _ := db.Begin()
	for id, rec := range records {
		if _, err := tx.Exec("INSERT INTO results (id, record) VALUES (?, ?)", id, rec); err != nil {
			b.Fatal(err)
		}
	}
	_ = tx.Commit()
	_ = db.Close()

	schema := kevSchema()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, err := OpenSQLiteGraph(dbPath, schema, testRender)
		if err != nil {
			b.Fatal(err)
		}
		if err := g.EagerScan(); err != nil {
			b.Fatal(err)
		}
		_ = g.Close()
	}
}

// ---------------------------------------------------------------------------
// Cross-reference tests (AddRef / FlushRefs / GetCallers)
// ---------------------------------------------------------------------------

func TestSQLiteGraph_AddRef_FlushRefs_GetCallers(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// Accumulate refs
	if err := g.AddRef("Println", "pkg/main/source"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddRef("Println", "pkg/util/source"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddRef("Sprintf", "pkg/main/source"); err != nil {
		t.Fatal(err)
	}

	// Flush to sidecar DB
	if err := g.FlushRefs(); err != nil {
		t.Fatal(err)
	}

	// Query callers of Println — should return both files
	nodes, err := g.GetCallers("Println")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("GetCallers(Println) = %d nodes, want 2", len(nodes))
	}
	paths := map[string]bool{}
	for _, n := range nodes {
		paths[n.ID] = true
	}
	if !paths["pkg/main/source"] || !paths["pkg/util/source"] {
		t.Errorf("unexpected paths: %v", paths)
	}

	// Query callers of Sprintf — should return 1 file
	nodes, err = g.GetCallers("Sprintf")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "pkg/main/source" {
		t.Errorf("GetCallers(Sprintf) = %v, want [pkg/main/source]", nodes)
	}

	// Nonexistent token — should return nil, nil
	nodes, err = g.GetCallers("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if nodes != nil {
		t.Errorf("GetCallers(nonexistent) = %v, want nil", nodes)
	}
}

func TestSQLiteGraph_FlushRefs_Idempotent(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	if err := g.AddRef("Println", "pkg/main/source"); err != nil {
		t.Fatal(err)
	}

	// First flush succeeds
	if err := g.FlushRefs(); err != nil {
		t.Fatal(err)
	}

	// Second flush is a no-op (sync.Once), no error
	if err := g.FlushRefs(); err != nil {
		t.Fatalf("second FlushRefs should be no-op, got: %v", err)
	}

	// Data from first flush is still intact
	nodes, err := g.GetCallers("Println")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("GetCallers after double flush = %d nodes, want 1", len(nodes))
	}
}

func TestSQLiteGraph_RefsDB_WipedOnOpen(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	// First session: add refs and flush
	g1, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	if err := g1.AddRef("Println", "pkg/main/source"); err != nil {
		t.Fatal(err)
	}
	if err := g1.FlushRefs(); err != nil {
		t.Fatal(err)
	}

	// Verify data exists before close
	nodes, err := g1.GetCallers("Println")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("session 1: GetCallers = %d, want 1", len(nodes))
	}
	_ = g1.Close()

	// Second session: re-open should wipe the stale .refs.db
	g2, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g2.Close() }()

	// Stale refs should be gone — clean slate
	nodes, err = g2.GetCallers("Println")
	if err != nil {
		t.Fatal(err)
	}
	if nodes != nil {
		t.Errorf("session 2: GetCallers should be nil after wipe, got %v", nodes)
	}
}

func TestSQLiteGraph_SizeCache_Invalidation(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// First GetNode renders content and populates sizeCache
	node1, err := g.GetNode("vulns/CVE-2024-0001/vendor")
	if err != nil {
		t.Fatal(err)
	}
	if string(node1.Data) != "Acme" {
		t.Fatalf("first GetNode data = %q, want %q", node1.Data, "Acme")
	}

	// Verify sizeCache is populated
	if _, ok := g.sizeCache.Load("vulns/CVE-2024-0001/vendor"); !ok {
		t.Fatal("sizeCache should have entry after first GetNode")
	}

	// Second GetNode should return lightweight node from sizeCache (no Data)
	node2, err := g.GetNode("vulns/CVE-2024-0001/vendor")
	if err != nil {
		t.Fatal(err)
	}
	if node2.Data != nil {
		t.Error("second GetNode should return lightweight node (nil Data)")
	}
	if node2.Ref == nil || node2.Ref.ContentLen != int64(len("Acme")) {
		t.Errorf("second GetNode should have Ref with ContentLen=4, got %v", node2.Ref)
	}

	// Invalidate clears the cache
	g.Invalidate("vulns/CVE-2024-0001/vendor")

	// Verify sizeCache is cleared
	if _, ok := g.sizeCache.Load("vulns/CVE-2024-0001/vendor"); ok {
		t.Fatal("sizeCache should be empty after Invalidate")
	}

	// Next GetNode should re-render (return Data again, not Ref)
	node3, err := g.GetNode("vulns/CVE-2024-0001/vendor")
	if err != nil {
		t.Fatal(err)
	}
	if string(node3.Data) != "Acme" {
		t.Fatalf("GetNode after Invalidate data = %q, want %q", node3.Data, "Acme")
	}
}

func TestSQLiteGraph_GetCallers_Lightweight(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	if err := g.AddRef("Println", "vulns/CVE-2024-0001/vendor"); err != nil {
		t.Fatal(err)
	}
	if err := g.FlushRefs(); err != nil {
		t.Fatal(err)
	}

	nodes, err := g.GetCallers("Println")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("GetCallers = %d nodes, want 1", len(nodes))
	}

	// Verify lightweight: Data should be nil (no eager content resolution)
	if nodes[0].Data != nil {
		t.Errorf("GetCallers returned node with Data populated (len=%d), want nil (lightweight)", len(nodes[0].Data))
	}
	if nodes[0].ID != "vulns/CVE-2024-0001/vendor" {
		t.Errorf("node ID = %q, want %q", nodes[0].ID, "vulns/CVE-2024-0001/vendor")
	}
}

// ---------------------------------------------------------------------------
// Virtual table (mache_refs) tests
// ---------------------------------------------------------------------------

func TestSQLiteGraph_VTab_MacheRefs(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"test"}}`,
		"CVE-2024-0002": `{"schema":"kev","identifier":"CVE-2024-0002","item":{"cveID":"CVE-2024-0002","vendorProject":"Globex","product":"Gizmo","shortDescription":"test2"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	// Add refs
	require.NoError(t, g.AddRef("MyFunc", "vulns/CVE-2024-0001/vendor"))
	require.NoError(t, g.AddRef("MyFunc", "vulns/CVE-2024-0002/vendor"))
	require.NoError(t, g.AddRef("Other", "vulns/CVE-2024-0001/vendor"))
	require.NoError(t, g.FlushRefs())

	// Each sub-test closes its rows before the next opens, avoiding
	// connection-pool exhaustion (refsDB MaxOpenConns=2: one for the
	// outer vtab query, one for Filter's inner queries).

	t.Run("token_lookup", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT path FROM mache_refs WHERE token = ?", "MyFunc")
		require.NoError(t, err)

		var paths []string
		for rows.Next() {
			var p string
			require.NoError(t, rows.Scan(&p))
			paths = append(paths, p)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()

		assert.Len(t, paths, 2)
		assert.Contains(t, paths, "vulns/CVE-2024-0001/vendor")
		assert.Contains(t, paths, "vulns/CVE-2024-0002/vendor")
	})

	t.Run("nonexistent_token", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT path FROM mache_refs WHERE token = ?", "Nope")
		require.NoError(t, err)
		hasRow := rows.Next()
		_ = rows.Close()
		assert.False(t, hasRow)
	})

	t.Run("like_query", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT token, path FROM mache_refs WHERE token LIKE ?", "My%")
		require.NoError(t, err)

		type ref struct{ token, path string }
		var refs []ref
		for rows.Next() {
			var r ref
			require.NoError(t, rows.Scan(&r.token, &r.path))
			refs = append(refs, r)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()

		// "MyFunc" matches "My%", "Other" does not
		assert.Len(t, refs, 2)
		for _, r := range refs {
			assert.Equal(t, "MyFunc", r.token)
		}
	})

	t.Run("glob_query", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT token, path FROM mache_refs WHERE token GLOB ?", "Other*")
		require.NoError(t, err)

		type ref struct{ token, path string }
		var refs []ref
		for rows.Next() {
			var r ref
			require.NoError(t, rows.Scan(&r.token, &r.path))
			refs = append(refs, r)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()

		assert.Len(t, refs, 1)
		assert.Equal(t, "Other", refs[0].token)
		assert.Equal(t, "vulns/CVE-2024-0001/vendor", refs[0].path)
	})

	t.Run("like_no_match", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT path FROM mache_refs WHERE token LIKE ?", "ZZZ%")
		require.NoError(t, err)
		hasRow := rows.Next()
		_ = rows.Close()
		assert.False(t, hasRow)
	})

	t.Run("full_scan", func(t *testing.T) {
		rows, err := g.QueryRefs("SELECT token, path FROM mache_refs")
		require.NoError(t, err)

		type ref struct{ token, path string }
		var allRefs []ref
		for rows.Next() {
			var r ref
			require.NoError(t, rows.Scan(&r.token, &r.path))
			allRefs = append(allRefs, r)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()

		// MyFunc→2 paths + Other→1 path = 3 total
		assert.Len(t, allRefs, 3)
	})
}

// ---------------------------------------------------------------------------
// Regression: custom table name (Bug: hardcoded "FROM results")
// ---------------------------------------------------------------------------

func TestSQLiteGraph_CustomTableName(t *testing.T) {
	dbPath := createTestDBWithTable(t, "vulnerabilities", map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme","product":"Widget","shortDescription":"RCE in Widget"}}`,
	})

	schema := &api.Topology{
		Version: "v1",
		Table:   "vulnerabilities",
		Nodes: []api.Node{
			{
				Name:     "vulns",
				Selector: "$",
				Children: []api.Node{
					{
						Name:     "{{.item.cveID}}",
						Selector: "$[*]",
						Files: []api.Leaf{
							{Name: "vendor", ContentTemplate: "{{.item.vendorProject}}"},
						},
					},
				},
			},
		},
	}

	g, err := OpenSQLiteGraph(dbPath, schema, testRender)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	require.NoError(t, g.EagerScan())

	children, err := g.ListChildren("vulns")
	require.NoError(t, err)
	assert.Contains(t, children, "vulns/CVE-2024-0001")

	// Verify content resolves through the custom table
	buf := make([]byte, 1024)
	n, err := g.ReadContent("vulns/CVE-2024-0001/vendor", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "Acme", string(buf[:n]))
}

func TestSQLiteGraph_DefaultTableName(t *testing.T) {
	// Schema with no Table field should default to "results"
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-0001": `{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Globex","product":"Gizmo","shortDescription":"SQLi in Gizmo"}}`,
	})

	g, err := OpenSQLiteGraph(dbPath, kevSchema(), testRender)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	require.NoError(t, g.EagerScan())

	children, err := g.ListChildren("vulns")
	require.NoError(t, err)
	assert.Contains(t, children, "vulns/CVE-2024-0001")
}

// ---------------------------------------------------------------------------
// Regression: HotSwapGraph closes old graph on Swap
// ---------------------------------------------------------------------------

type closableGraph struct {
	Graph
	closed bool
}

func (c *closableGraph) Close() error {
	c.closed = true
	return nil
}

func TestHotSwapGraph_ClosesOldOnSwap(t *testing.T) {
	store1 := &closableGraph{Graph: NewMemoryStore()}
	store2 := &closableGraph{Graph: NewMemoryStore()}

	hot := NewHotSwapGraph(store1)

	// Swap should close the old graph
	hot.Swap(store2)
	assert.True(t, store1.closed, "old graph should be closed after Swap")
	assert.False(t, store2.closed, "new graph should not be closed")

	// Second swap should close store2
	store3 := &closableGraph{Graph: NewMemoryStore()}
	hot.Swap(store3)
	assert.True(t, store2.closed, "second old graph should be closed after Swap")
	assert.False(t, store3.closed, "current graph should not be closed")
}
