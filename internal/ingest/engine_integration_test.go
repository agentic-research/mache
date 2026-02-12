package ingest

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadSchema(t *testing.T, path string) *api.Topology {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var topo api.Topology
	require.NoError(t, json.Unmarshal(data, &topo))
	return &topo
}

func TestEngine_IngestSQLite_KEV(t *testing.T) {
	dbPath := createTestDB(t, []string{
		`{"schema":"kev","identifier":"CVE-2024-0001","item":{"cveID":"CVE-2024-0001","vendorProject":"Acme Corp","product":"Widget","shortDescription":"Remote code execution in Widget","dateAdded":"2024-01-15","dueDate":"2024-02-15"}}`,
		`{"schema":"kev","identifier":"CVE-2024-0002","item":{"cveID":"CVE-2024-0002","vendorProject":"Globex","product":"Gizmo","shortDescription":"SQL injection in Gizmo","dateAdded":"2024-02-01","dueDate":"2024-03-01"}}`,
		`{"schema":"kev","identifier":"CVE-2024-0003","item":{"cveID":"CVE-2024-0003","vendorProject":"Initech","product":"TPS Report","shortDescription":"XSS in TPS Report viewer","dateAdded":"2024-03-01","dueDate":"2024-04-01"}}`,
	})

	schema := loadSchema(t, "../../examples/kev-schema.json")
	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	require.NoError(t, engine.Ingest(dbPath))

	// vulns root dir exists with 3 children
	vulns, err := store.GetNode("vulns")
	require.NoError(t, err)
	assert.True(t, vulns.Mode.IsDir())
	assert.Len(t, vulns.Children, 3)

	// Spot-check first CVE files
	vendor, err := store.GetNode("vulns/CVE-2024-0001/vendor")
	require.NoError(t, err)
	assert.Equal(t, "Acme Corp", string(vendor.Data))

	product, err := store.GetNode("vulns/CVE-2024-0001/product")
	require.NoError(t, err)
	assert.Equal(t, "Widget", string(product.Data))

	desc, err := store.GetNode("vulns/CVE-2024-0001/description")
	require.NoError(t, err)
	assert.Equal(t, "Remote code execution in Widget", string(desc.Data))

	// raw.json is valid JSON containing the full record
	raw, err := store.GetNode("vulns/CVE-2024-0001/raw.json")
	require.NoError(t, err)
	var rawData map[string]any
	require.NoError(t, json.Unmarshal(raw.Data, &rawData))
	assert.Equal(t, "CVE-2024-0001", rawData["identifier"])
}

func TestEngine_IngestSQLite_NVD(t *testing.T) {
	dbPath := createTestDB(t, []string{
		`{"schema":"nvd","identifier":"CVE-2024-0001","item":{"cve":{"id":"CVE-2024-0001","descriptions":[{"lang":"en","value":"A buffer overflow in FooBar allows remote code execution."}],"published":"2024-01-15T00:00:00Z","vulnStatus":"Analyzed"}}}`,
		`{"schema":"nvd","identifier":"CVE-2024-0002","item":{"cve":{"id":"CVE-2024-0002","descriptions":[{"lang":"en","value":"An authentication bypass in BazQux."}],"published":"2024-02-01T00:00:00Z","vulnStatus":"Modified"}}}`,
	})

	schema := loadSchema(t, "../../examples/nvd-schema.json")
	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	require.NoError(t, engine.Ingest(dbPath))

	// by-cve root dir exists with 2 children
	byCve, err := store.GetNode("by-cve")
	require.NoError(t, err)
	assert.True(t, byCve.Mode.IsDir())
	assert.Len(t, byCve.Children, 2)

	// description uses nested template with index
	desc, err := store.GetNode("by-cve/CVE-2024-0001/description")
	require.NoError(t, err)
	assert.Equal(t, "A buffer overflow in FooBar allows remote code execution.", string(desc.Data))

	// status from nested path
	status, err := store.GetNode("by-cve/CVE-2024-0001/status")
	require.NoError(t, err)
	assert.Equal(t, "Analyzed", string(status.Data))

	// raw.json is valid JSON
	raw, err := store.GetNode("by-cve/CVE-2024-0001/raw.json")
	require.NoError(t, err)
	var rawData map[string]any
	require.NoError(t, json.Unmarshal(raw.Data, &rawData))
	item := rawData["item"].(map[string]any)
	cve := item["cve"].(map[string]any)
	assert.Equal(t, "CVE-2024-0001", cve["id"])
}
