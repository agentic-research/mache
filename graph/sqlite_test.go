package graph_test

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
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
		Properties: map[string][]byte{"mache_id": []byte("mache-42")},
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
	if string(feedNode.Properties["mache_id"]) != "mache-42" {
		t.Errorf("feed.Properties[mache_id] = %q, want mache-42", feedNode.Properties["mache_id"])
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
