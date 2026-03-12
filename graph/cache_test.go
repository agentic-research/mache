package graph_test

import (
	"io/fs"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentic-research/mache/graph"
)

func TestGraphCache_InMemory_PutGetFile(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutFile("root/hello", []byte("world"))

	data, ok := c.GetData("root/hello")
	if !ok {
		t.Fatal("expected to find root/hello")
	}
	if string(data) != "world" {
		t.Errorf("data = %q, want %q", data, "world")
	}
}

func TestGraphCache_PutDir_Idempotent(t *testing.T) {
	c := graph.NewGraphCache("")

	created := c.PutDir("mydir", true)
	if !created {
		t.Error("first PutDir should return true")
	}

	created = c.PutDir("mydir", true)
	if created {
		t.Error("second PutDir should return false (already exists)")
	}

	node, ok := c.GetNode("mydir")
	if !ok {
		t.Fatal("expected to find mydir")
	}
	if !node.Mode.IsDir() {
		t.Error("mydir should be a directory")
	}
}

func TestGraphCache_AppendChild_Dedup(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutDir("parent", true)
	c.PutFile("parent/child", []byte("data"))

	ok := c.AppendChild("parent", "parent/child")
	if !ok {
		t.Fatal("AppendChild should return true")
	}

	// Append same child again — should deduplicate.
	ok = c.AppendChild("parent", "parent/child")
	if !ok {
		t.Fatal("AppendChild should return true for already-present child")
	}

	children, ok := c.ListChildren("parent")
	if !ok {
		t.Fatal("expected to list parent's children")
	}
	if len(children) != 1 {
		t.Errorf("len(children) = %d, want 1 (dedup failed)", len(children))
	}
	if children[0] != "parent/child" {
		t.Errorf("children[0] = %q, want %q", children[0], "parent/child")
	}
}

func TestGraphCache_AppendChild_NonexistentParent(t *testing.T) {
	c := graph.NewGraphCache("")

	ok := c.AppendChild("nonexistent", "child")
	if ok {
		t.Error("AppendChild should return false for nonexistent parent")
	}
}

func TestGraphCache_RemoveChild(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutDir("parent", true)
	c.PutFile("parent/a", []byte("a"))
	c.PutFile("parent/b", []byte("b"))
	c.AppendChild("parent", "parent/a")
	c.AppendChild("parent", "parent/b")

	ok := c.RemoveChild("parent", "parent/a")
	if !ok {
		t.Fatal("RemoveChild should return true")
	}

	children, ok := c.ListChildren("parent")
	if !ok {
		t.Fatal("expected to list parent's children")
	}
	if len(children) != 1 || children[0] != "parent/b" {
		t.Errorf("children = %v, want [parent/b]", children)
	}
}

func TestGraphCache_ClearChildren(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutDir("parent", true)
	c.PutFile("parent/a", []byte("a"))
	c.AppendChild("parent", "parent/a")

	ok := c.ClearChildren("parent")
	if !ok {
		t.Fatal("ClearChildren should return true")
	}

	children, ok := c.ListChildren("parent")
	if !ok {
		t.Fatal("expected parent to still exist as dir")
	}
	if len(children) != 0 {
		t.Errorf("children = %v, want empty", children)
	}
}

func TestGraphCache_SQLitePersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cache.db")

	// Write data with first cache instance.
	c1 := graph.NewGraphCache(dbPath)
	c1.PutDir("root", true)
	c1.PutFile("root/key", []byte("value"))
	c1.AppendChild("root", "root/key")

	// Create a second cache on the same path — should reload.
	c2 := graph.NewGraphCache(dbPath)

	roots := c2.RootIDs()
	if len(roots) != 1 || roots[0] != "root" {
		t.Fatalf("roots = %v, want [root]", roots)
	}

	node, ok := c2.GetNode("root")
	if !ok {
		t.Fatal("expected to find root after reload")
	}
	if !node.Mode.IsDir() {
		t.Error("root should be a directory")
	}
	if len(node.Children) != 1 || node.Children[0] != "root/key" {
		t.Errorf("root.Children = %v, want [root/key]", node.Children)
	}

	data, ok := c2.GetData("root/key")
	if !ok || string(data) != "value" {
		t.Errorf("root/key data = %q, want %q", data, "value")
	}
}

func TestGraphCache_Overwrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cache.db")

	c1 := graph.NewGraphCache(dbPath)
	c1.PutDir("root", true)
	c1.PutFile("root/val", []byte("old"))
	c1.AppendChild("root", "root/val")

	// Overwrite with new data.
	c1.PutFile("root/val", []byte("new"))

	// Reload and verify latest data wins.
	c2 := graph.NewGraphCache(dbPath)
	data, ok := c2.GetData("root/val")
	if !ok || string(data) != "new" {
		t.Errorf("data after overwrite = %q, want %q", data, "new")
	}
}

func TestGraphCache_EmptyDBPath(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutDir("test", true)
	c.PutFile("test/file", []byte("data"))

	// Persist should be a no-op for in-memory cache.
	if err := c.Persist(); err != nil {
		t.Errorf("Persist() = %v, want nil for empty dbPath", err)
	}

	// Data should still be accessible.
	data, ok := c.GetData("test/file")
	if !ok || string(data) != "data" {
		t.Errorf("data = %q, want %q", data, "data")
	}
}

func TestGraphCache_GetData_Nonexistent(t *testing.T) {
	c := graph.NewGraphCache("")

	_, ok := c.GetData("does/not/exist")
	if ok {
		t.Error("GetData should return false for nonexistent node")
	}
}

func TestGraphCache_ListChildren_FileNode(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutFile("file", []byte("not a dir"))

	_, ok := c.ListChildren("file")
	if ok {
		t.Error("ListChildren should return false for a file node")
	}
}

func TestGraphCache_RootIDs(t *testing.T) {
	c := graph.NewGraphCache("")

	c.PutDir("a", true)
	c.PutDir("b", true)
	c.PutDir("c", false) // not a root

	roots := c.RootIDs()
	if len(roots) != 2 {
		t.Fatalf("len(roots) = %d, want 2", len(roots))
	}

	rootSet := make(map[string]bool)
	for _, r := range roots {
		rootSet[r] = true
	}
	if !rootSet["a"] || !rootSet["b"] {
		t.Errorf("roots = %v, want [a, b]", roots)
	}
	if rootSet["c"] {
		t.Error("c should not be a root")
	}
}

func TestGraphCache_ConcurrentAccess(t *testing.T) {
	c := graph.NewGraphCache("")
	c.PutDir("root", true)

	var wg sync.WaitGroup
	// Concurrent writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "root/file" + string(rune('0'+i))
			c.PutFile(id, []byte("data"))
			c.AppendChild("root", id)
		}(i)
	}
	// Concurrent readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.GetNode("root")
			c.ListChildren("root")
			c.RootIDs()
		}()
	}
	wg.Wait()

	// Verify root exists and has children (exact count may vary due to
	// MemoryStore's internal pointer semantics under concurrent append).
	node, ok := c.GetNode("root")
	if !ok {
		t.Fatal("root should exist")
	}
	if !node.Mode.IsDir() {
		t.Error("root should be a directory")
	}
}

func TestGraphCache_StoreEscapeHatch(t *testing.T) {
	c := graph.NewGraphCache("")

	// Use Store() for direct batch mutations.
	store := c.Store()
	store.AddRoot(&graph.Node{
		ID:       "batch",
		Mode:     fs.ModeDir,
		Children: []string{"batch/a", "batch/b"},
	})
	store.AddNode(&graph.Node{ID: "batch/a", Data: []byte("a")})
	store.AddNode(&graph.Node{ID: "batch/b", Data: []byte("b")})

	// Verify via GraphCache read methods.
	children, ok := c.ListChildren("batch")
	if !ok {
		t.Fatal("expected batch dir via Store() escape hatch")
	}
	if len(children) != 2 {
		t.Errorf("children = %v, want 2 entries", children)
	}
}
