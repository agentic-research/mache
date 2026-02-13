package lattice

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestInferFromRecords_KEVLike(t *testing.T) {
	records := makeKEVRecords(5)
	inf := &Inferrer{Config: InferConfig{SampleSize: 100, RootName: "vulns"}}

	topo, err := inf.InferFromRecords(records)
	require.NoError(t, err)
	require.NotNil(t, topo)

	assert.Equal(t, "v1", topo.Version)
	require.Len(t, topo.Nodes, 1)

	root := topo.Nodes[0]
	assert.Equal(t, "vulns", root.Name)

	// Flat schema: no deep nesting (identifier is direct child of root)
	idLevel := root.Children[0]
	// Should have files directly or at most one level of children
	node := findInnermostNode(root)
	assert.NotEmpty(t, node.Files, "should have leaf files")

	// Verify raw.json is present
	hasRaw := false
	for _, f := range node.Files {
		if f.Name == "raw.json" {
			hasRaw = true
		}
	}
	assert.True(t, hasRaw)

	// The identifier template should reference a high-cardinality field
	assert.Contains(t, idLevel.Name, "{{")
}

func TestInferFromRecords_NVDLike(t *testing.T) {
	records := makeNVDRecords(10)
	inf := &Inferrer{Config: InferConfig{SampleSize: 100, RootName: "by-cve"}}

	topo, err := inf.InferFromRecords(records)
	require.NoError(t, err)
	require.NotNil(t, topo)

	root := topo.Nodes[0]
	assert.Equal(t, "by-cve", root.Name)

	// Should have temporal sharding: at least 2 levels of nesting
	// before reaching the identifier+files level
	depth := nodeDepth(root)
	assert.GreaterOrEqual(t, depth, 3, "NVD-like data should produce sharded topology (year/month/id)")

	// The innermost node should have files
	node := findInnermostNode(root)
	assert.NotEmpty(t, node.Files)
}

func TestInferFromSQLite_Synthetic(t *testing.T) {
	// Create a temporary SQLite database with synthetic records
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT)")
	require.NoError(t, err)

	records := makeKEVRecords(20)
	for i, rec := range records {
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = db.Exec("INSERT INTO results (id, record) VALUES (?, ?)",
			fmt.Sprintf("rec-%d", i), string(data))
		require.NoError(t, err)
	}
	_ = db.Close()

	// Run full pipeline
	inf := &Inferrer{Config: InferConfig{SampleSize: 100, RootName: "vulns"}}
	topo, err := inf.InferFromSQLite(dbPath)
	require.NoError(t, err)
	require.NotNil(t, topo)

	assert.Equal(t, "v1", topo.Version)
	require.Len(t, topo.Nodes, 1)
	assert.Equal(t, "vulns", topo.Nodes[0].Name)

	// Should produce a valid schema that can be serialized
	data, err := json.MarshalIndent(topo, "", "  ")
	require.NoError(t, err)
	assert.Contains(t, string(data), "raw.json")
}

func TestInferFromSQLite_Integration_KEV(t *testing.T) {
	dbPath := os.Getenv("MACHE_TEST_KEV_DB")
	if dbPath == "" {
		t.Skip("MACHE_TEST_KEV_DB not set")
	}

	inf := &Inferrer{Config: InferConfig{SampleSize: 500, RootName: "vulns"}}
	topo, err := inf.InferFromSQLite(dbPath)
	require.NoError(t, err)
	require.NotNil(t, topo)

	data, err := json.MarshalIndent(topo, "", "  ")
	require.NoError(t, err)
	t.Logf("Inferred KEV schema:\n%s", string(data))

	// Basic structure checks
	require.Len(t, topo.Nodes, 1)
	assert.Equal(t, "vulns", topo.Nodes[0].Name)
	node := findInnermostNode(topo.Nodes[0])
	assert.NotEmpty(t, node.Files)
}

func TestInferFromSQLite_Integration_NVD(t *testing.T) {
	dbPath := os.Getenv("MACHE_TEST_NVD_DB")
	if dbPath == "" {
		t.Skip("MACHE_TEST_NVD_DB not set")
	}

	inf := &Inferrer{Config: InferConfig{SampleSize: 1000, RootName: "by-cve"}}
	topo, err := inf.InferFromSQLite(dbPath)
	require.NoError(t, err)
	require.NotNil(t, topo)

	data, err := json.MarshalIndent(topo, "", "  ")
	require.NoError(t, err)
	t.Logf("Inferred NVD schema:\n%s", string(data))

	require.Len(t, topo.Nodes, 1)
	depth := nodeDepth(topo.Nodes[0])
	assert.GreaterOrEqual(t, depth, 3, "NVD should produce temporal sharding")
}

// nodeDepth returns the maximum depth of a node tree.
func nodeDepth(n api.Node) int {
	if len(n.Children) == 0 {
		return 1
	}
	maxChild := 0
	for _, c := range n.Children {
		d := nodeDepth(c)
		if d > maxChild {
			maxChild = d
		}
	}
	return 1 + maxChild
}
