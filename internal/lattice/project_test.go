package lattice

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProject_FlatSchema(t *testing.T) {
	// KEV-like: uniform records with unique ID, no date sharding
	records := makeKEVRecords(5)
	ctx := BuildContextFromRecords(records)
	concepts := NextClosure(ctx)

	topo := Project(concepts, ctx, ProjectConfig{RootName: "vulns"})
	require.NotNil(t, topo)
	assert.Equal(t, "v1", topo.Version)
	require.Len(t, topo.Nodes, 1)

	root := topo.Nodes[0]
	assert.Equal(t, "vulns", root.Name)
	assert.Equal(t, "$", root.Selector)
	require.NotEmpty(t, root.Children)

	// Flat schema: root → identifier level (no shard nesting)
	// The identifier level should have files
	idLevel := root.Children[0]
	assert.Contains(t, idLevel.Name, "{{")
	assert.NotEmpty(t, idLevel.Files)

	// Check raw.json is present
	hasRaw := false
	for _, f := range idLevel.Files {
		if f.Name == "raw.json" {
			hasRaw = true
			assert.Equal(t, "{{. | json}}", f.ContentTemplate)
		}
	}
	assert.True(t, hasRaw, "should include raw.json")
}

func TestProject_TemporalSharding(t *testing.T) {
	// NVD-like: records with date field → year/month sharding
	records := makeNVDRecords(10)
	ctx := BuildContextFromRecords(records)
	concepts := NextClosure(ctx)

	topo := Project(concepts, ctx, ProjectConfig{RootName: "by-cve"})
	require.NotNil(t, topo)
	require.Len(t, topo.Nodes, 1)

	root := topo.Nodes[0]
	assert.Equal(t, "by-cve", root.Name)
	require.NotEmpty(t, root.Children)

	// Should have shard levels (year → month → identifier)
	yearLevel := root.Children[0]
	assert.Contains(t, yearLevel.Name, "slice")
	assert.Contains(t, yearLevel.Name, "0 4") // year slice

	require.NotEmpty(t, yearLevel.Children)
	monthLevel := yearLevel.Children[0]
	assert.Contains(t, monthLevel.Name, "slice")
	assert.Contains(t, monthLevel.Name, "5 7") // month slice

	require.NotEmpty(t, monthLevel.Children)
	idLevel := monthLevel.Children[0]
	assert.Contains(t, idLevel.Name, "{{")
	assert.NotEmpty(t, idLevel.Files)
}

func TestProject_IdentifierDetection(t *testing.T) {
	// The highest-cardinality universal field should be chosen as dir name
	records := []any{
		map[string]any{"id": "alpha", "status": "open", "category": "bug"},
		map[string]any{"id": "beta", "status": "closed", "category": "feature"},
		map[string]any{"id": "gamma", "status": "open", "category": "bug"},
		map[string]any{"id": "delta", "status": "open", "category": "task"},
	}
	ctx := BuildContextFromRecords(records)
	concepts := NextClosure(ctx)

	topo := Project(concepts, ctx, DefaultProjectConfig())
	require.NotNil(t, topo)

	// The identifier level should reference "id" (4 unique values for 4 records)
	root := topo.Nodes[0]
	// Walk to find the innermost node with files
	node := findInnermostNode(root)
	assert.Contains(t, node.Name, "id", "identifier should be the id field")
}

func TestProject_LeafFileGeneration(t *testing.T) {
	records := makeKEVRecords(3)
	ctx := BuildContextFromRecords(records)
	concepts := NextClosure(ctx)

	topo := Project(concepts, ctx, ProjectConfig{RootName: "vulns"})
	require.NotNil(t, topo)

	// Find the node with files
	node := findInnermostNode(topo.Nodes[0])

	// Should have leaf files for universal scalar fields + raw.json
	fileNames := make(map[string]bool)
	for _, f := range node.Files {
		fileNames[f.Name] = true
		// Each content template should reference a field
		if f.Name != "raw.json" {
			assert.Contains(t, f.ContentTemplate, "{{.")
		}
	}
	assert.True(t, fileNames["raw.json"], "must include raw.json")
	// Should have at least one non-raw file (vendor, product, etc.)
	assert.True(t, len(node.Files) > 1, "should have leaf files beyond raw.json")
}

func TestProject_OutputIsValidTopology(t *testing.T) {
	records := makeKEVRecords(5)
	ctx := BuildContextFromRecords(records)
	concepts := NextClosure(ctx)

	topo := Project(concepts, ctx, ProjectConfig{RootName: "vulns"})

	// Marshal to JSON and back — must round-trip cleanly
	data, err := json.Marshal(topo)
	require.NoError(t, err)

	var roundTripped api.Topology
	err = json.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, topo.Version, roundTripped.Version)
	assert.Len(t, roundTripped.Nodes, len(topo.Nodes))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeKEVRecords(n int) []any {
	records := make([]any, n)
	vendors := []string{"Acme", "BetaCorp", "GammaInc", "DeltaLLC", "EpsilonSys"}
	products := []string{"Widget", "Gadget", "Sprocket", "Thingamajig", "Doohickey"}
	for i := 0; i < n; i++ {
		records[i] = map[string]any{
			"schema":     "kev",
			"identifier": fmt.Sprintf("CVE-2024-%04d", i+1),
			"item": map[string]any{
				"cveID":            fmt.Sprintf("CVE-2024-%04d", i+1),
				"vendorProject":    vendors[i%len(vendors)],
				"product":          products[i%len(products)],
				"shortDescription": fmt.Sprintf("Vulnerability %d description", i+1),
				"dateAdded":        fmt.Sprintf("2024-01-%02d", (i%28)+1),
				"dueDate":          fmt.Sprintf("2024-02-%02d", (i%28)+1),
			},
		}
	}
	return records
}

func makeNVDRecords(n int) []any {
	records := make([]any, n)
	years := []string{"2023", "2024"}
	months := []string{"01", "03", "06", "09", "11"}
	statuses := []string{"Analyzed", "Modified", "Awaiting Analysis"}
	for i := 0; i < n; i++ {
		y := years[i%len(years)]
		m := months[i%len(months)]
		records[i] = map[string]any{
			"schema":     "nvd",
			"identifier": fmt.Sprintf("CVE-%s-%04d", y, i+1),
			"item": map[string]any{
				"cve": map[string]any{
					"id":         fmt.Sprintf("CVE-%s-%04d", y, i+1),
					"published":  fmt.Sprintf("%s-%s-15T00:00:00", y, m),
					"vulnStatus": statuses[i%len(statuses)],
				},
			},
		}
	}
	return records
}

func findInnermostNode(n api.Node) api.Node {
	if len(n.Children) == 0 {
		return n
	}
	return findInnermostNode(n.Children[0])
}
