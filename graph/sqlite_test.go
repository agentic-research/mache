package graph_test

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/require"
)

func TestExportImportRoundTrip(t *testing.T) {
	// Build a small in-memory graph simulating x-ray's mache engine.
	store := graph.NewMemoryStore()

	root := &graph.Node{
		ID:       "main",
		Mode:     fs.ModeDir,
		Children: []string{"main/feed"},
	}
	store.AddRoot(root)

	feed := &graph.Node{
		ID:         "main/feed",
		Mode:       fs.ModeDir,
		Children:   []string{"main/feed/description", "main/feed/mache_id"},
		Properties: map[string]json.RawMessage{"mache_id": []byte(`"mache-42"`)},
	}
	store.AddNode(feed)

	store.AddNode(&graph.Node{
		ID:   "main/feed/description",
		Data: []byte("Main content feed with stories"),
	})
	store.AddNode(&graph.Node{
		ID:   "main/feed/mache_id",
		Data: []byte("mache-42"),
	})

	// Export to SQLite.
	dbPath := filepath.Join(t.TempDir(), "test-graph.db")
	if err := graph.ExportSQLite(store, dbPath); err != nil {
		t.Fatalf("ExportSQLite: %v", err)
	}

	// Import from SQLite.
	imported, err := graph.ImportSQLite(dbPath)
	if err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}

	// Verify root.
	roots := imported.RootIDs()
	if len(roots) != 1 || roots[0] != "main" {
		t.Fatalf("expected roots=[main], got %v", roots)
	}

	// Verify directory structure.
	mainNode, err := imported.GetNode("main")
	if err != nil {
		t.Fatalf("GetNode(main): %v", err)
	}
	if !mainNode.Mode.IsDir() {
		t.Error("main should be a directory")
	}
	if len(mainNode.Children) != 1 || mainNode.Children[0] != "main/feed" {
		t.Errorf("main.Children = %v, want [main/feed]", mainNode.Children)
	}

	// Verify feed node with properties.
	feedNode, err := imported.GetNode("main/feed")
	if err != nil {
		t.Fatalf("GetNode(main/feed): %v", err)
	}
	if got := graph.PropString(feedNode, "mache_id"); got != "mache-42" {
		t.Errorf("feed.Properties[mache_id] = %q, want mache-42", got)
	}
	if len(feedNode.Children) != 2 {
		t.Errorf("feed.Children = %v, want 2 children", feedNode.Children)
	}

	// Verify file nodes.
	desc, err := imported.GetNode("main/feed/description")
	if err != nil {
		t.Fatalf("GetNode(main/feed/description): %v", err)
	}
	if string(desc.Data) != "Main content feed with stories" {
		t.Errorf("description data = %q", desc.Data)
	}

	mid, err := imported.GetNode("main/feed/mache_id")
	if err != nil {
		t.Fatalf("GetNode(main/feed/mache_id): %v", err)
	}
	if string(mid.Data) != "mache-42" {
		t.Errorf("mache_id data = %q", mid.Data)
	}
}

func TestExportImportEmptyStore(t *testing.T) {
	store := graph.NewMemoryStore()
	dbPath := filepath.Join(t.TempDir(), "empty.db")

	if err := graph.ExportSQLite(store, dbPath); err != nil {
		t.Fatalf("ExportSQLite: %v", err)
	}

	imported, err := graph.ImportSQLite(dbPath)
	if err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}

	if len(imported.RootIDs()) != 0 {
		t.Errorf("expected 0 roots, got %d", len(imported.RootIDs()))
	}
}

// newFixtureDB builds a mache-shaped .db via internal/fixturedb — never
// hand-written DDL (internal/lint's LLO boundary rule forbids test files
// from hand-typing node_defs/node_refs; a hand-written CREATE TABLE is a
// hidden test parameter, since ensureCanonicalViews emits a structurally
// different v_refs/v_defs per column combination). fixturedb.Leyline derives
// the exact shape from the real pinned producer.
func newFixtureDB(t *testing.T) string {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("dedupSuffix", "functions/dedupSuffix", fixturedb.Function)
	b.Ref("dedupSuffix", "functions/caller", "functions/caller/call_0", "")
	dbPath, _ := b.Build()
	return dbPath
}

// TestOpen_LookupDefWorksWithoutImport is the regression test for the bug
// this session found: a consumer loading a mache .db via
// MemoryStore+ImportSQLite got LookupDef returning nil and QueryRefs
// erroring "refsDB not initialized" — not because the .db lacked the data,
// but because ImportSQLite only replicates the node tree and never touches
// node_defs/node_refs. Open (backed by SQLiteGraph) reads those tables
// directly, so both work immediately with no import/populate step.
func TestOpen_LookupDefWorksWithoutImport(t *testing.T) {
	dbPath := newFixtureDB(t)

	g, err := graph.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	ids := g.LookupDef("dedupSuffix")
	require.Equal(t, []string{"functions/dedupSuffix"}, ids,
		"LookupDef must resolve directly from node_defs, no AddDef call required")

	require.Nil(t, g.LookupDef("NotInTheTable"),
		"an absent token must return nil, not a phantom match")
}

// TestOpen_QueryRefsWorksWithoutImport is QueryRefs' half of the same
// regression: querying node_refs must work directly against the file.
func TestOpen_QueryRefsWorksWithoutImport(t *testing.T) {
	dbPath := newFixtureDB(t)

	g, err := graph.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	rows, err := g.QueryRefs("SELECT node_id FROM node_refs WHERE token = ?", "dedupSuffix")
	require.NoError(t, err, "QueryRefs must not require an explicit InitRefsDB/FlushRefs step")
	defer func() { _ = rows.Close() }()

	var nodeIDs []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		nodeIDs = append(nodeIDs, id)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"functions/caller/call_0"}, nodeIDs)
}

// TestImportSQLite_DoesNotWireLookupDefOrQueryRefs pins ImportSQLite's
// documented limitation so a future change either fixes it deliberately
// (updating this test) or is caught here rather than rediscovered blind by
// the next consumer. Use Open, not ImportSQLite, when LookupDef/QueryRefs
// need to work.
func TestImportSQLite_DoesNotWireLookupDefOrQueryRefs(t *testing.T) {
	dbPath := newFixtureDB(t)

	imported, err := graph.ImportSQLite(dbPath)
	require.NoError(t, err)

	require.Nil(t, imported.LookupDef("dedupSuffix"),
		"ImportSQLite does not read node_defs — LookupDef stays empty until this is fixed or documented otherwise")

	_, err = imported.QueryRefs("SELECT 1")
	require.Error(t, err, "ImportSQLite does not call InitRefsDB — QueryRefs must still error")
}
