package graph

import (
	"io/fs"
	"sort"
	"testing"
	"time"
)

// helper: build a small MemoryStore with known nodes.
func testStore(rootID string, children map[string][]string, files map[string][]byte) *MemoryStore {
	s := NewMemoryStore()
	root := &Node{
		ID:      rootID,
		Mode:    fs.ModeDir | 0o555,
		ModTime: time.Now(),
	}
	if kids, ok := children[rootID]; ok {
		root.Children = kids
	}
	s.AddRoot(root)

	for id, kids := range children {
		if id == rootID {
			continue
		}
		s.AddNode(&Node{
			ID:       id,
			Mode:     fs.ModeDir | 0o555,
			ModTime:  time.Now(),
			Children: kids,
		})
	}

	for id, data := range files {
		s.AddNode(&Node{
			ID:      id,
			Mode:    0o444,
			ModTime: time.Now(),
			Data:    data,
		})
	}
	return s
}

func TestCompositeGraph_RootListsMounts(t *testing.T) {
	c := NewCompositeGraph()
	s1 := testStore("zone1", nil, nil)
	s2 := testStore("zone2", nil, nil)

	if err := c.Mount("browser", s1); err != nil {
		t.Fatal(err)
	}
	if err := c.Mount("iterm", s2); err != nil {
		t.Fatal(err)
	}

	children, err := c.ListChildren("")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(children)
	if len(children) != 2 || children[0] != "browser" || children[1] != "iterm" {
		t.Fatalf("expected [browser iterm], got %v", children)
	}
}

func TestCompositeGraph_MountDuplicate(t *testing.T) {
	c := NewCompositeGraph()
	s := testStore("x", nil, nil)
	if err := c.Mount("browser", s); err != nil {
		t.Fatal(err)
	}
	if err := c.Mount("browser", s); err == nil {
		t.Fatal("expected error on duplicate mount")
	}
}

func TestCompositeGraph_Unmount(t *testing.T) {
	c := NewCompositeGraph()
	s := testStore("x", nil, nil)
	_ = c.Mount("browser", s)

	if err := c.Unmount("browser"); err != nil {
		t.Fatal(err)
	}
	children, _ := c.ListChildren("")
	if len(children) != 0 {
		t.Fatalf("expected empty after unmount, got %v", children)
	}
	if err := c.Unmount("browser"); err == nil {
		t.Fatal("expected error unmounting non-existent mount")
	}
}

func TestCompositeGraph_GetNodeRoot(t *testing.T) {
	c := NewCompositeGraph()
	_ = c.Mount("browser", testStore("x", nil, nil))

	node, err := c.GetNode("")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Mode.IsDir() {
		t.Fatal("root should be a directory")
	}
}

func TestCompositeGraph_GetNodeMountPoint(t *testing.T) {
	c := NewCompositeGraph()
	_ = c.Mount("browser", testStore("x", nil, nil))

	node, err := c.GetNode("browser")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Mode.IsDir() {
		t.Fatal("mount point should be a directory")
	}
}

func TestCompositeGraph_PathRouting(t *testing.T) {
	browserStore := testStore("header",
		map[string][]string{
			"header": {"header/nav"},
		},
		map[string][]byte{
			"header/nav": []byte("navigation bar"),
		},
	)
	itermStore := testStore("windows",
		map[string][]string{
			"windows": {"windows/session1"},
		},
		map[string][]byte{
			"windows/session1": []byte("$ ls -la"),
		},
	)

	c := NewCompositeGraph()
	_ = c.Mount("browser", browserStore)
	_ = c.Mount("iterm", itermStore)

	// Read from browser mount
	buf := make([]byte, 100)
	n, err := c.ReadContent("browser/header/nav", buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "navigation bar" {
		t.Fatalf("expected 'navigation bar', got %q", buf[:n])
	}

	// Read from iterm mount
	n, err = c.ReadContent("iterm/windows/session1", buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "$ ls -la" {
		t.Fatalf("expected '$ ls -la', got %q", buf[:n])
	}

	// ListChildren through mount
	children, err := c.ListChildren("browser/header")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0] != "browser/header/nav" {
		t.Fatalf("expected [browser/header/nav], got %v", children)
	}

	// GetNode through mount
	node, err := c.GetNode("iterm/windows")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Mode.IsDir() {
		t.Fatal("windows should be a directory")
	}
}

func TestCompositeGraph_NotFound(t *testing.T) {
	c := NewCompositeGraph()
	_ = c.Mount("browser", testStore("x", nil, nil))

	_, err := c.GetNode("nonexistent/path")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCompositeGraph_ListChildrenAtMountRoot(t *testing.T) {
	s := testStore("zone1",
		map[string][]string{
			"zone1": {"zone1/file1"},
		},
		map[string][]byte{
			"zone1/file1": []byte("content"),
		},
	)
	c := NewCompositeGraph()
	_ = c.Mount("browser", s)

	// ListChildren at mount root delegates to sub-graph's root
	children, err := c.ListChildren("browser")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0] != "browser/zone1" {
		t.Fatalf("expected [browser/zone1], got %v", children)
	}
}

func TestCompositeGraph_ActRouting(t *testing.T) {
	s := testStore("x", nil, nil)
	c := NewCompositeGraph()
	_ = c.Mount("browser", s)

	// MemoryStore returns ErrActNotSupported
	_, err := c.Act("browser/x", "click", "")
	if err != ErrActNotSupported {
		t.Fatalf("expected ErrActNotSupported, got %v", err)
	}
}

func TestCompositeGraph_InvalidateRoutes(t *testing.T) {
	s := testStore("x", nil, map[string][]byte{
		"x/file": []byte("data"),
	})
	c := NewCompositeGraph()
	_ = c.Mount("m", s)

	// Should not panic — just delegates to sub-graph
	c.Invalidate("m/x/file")
	c.Invalidate("nonexistent/path") // no-op for unknown mount
}

// delegatingGraph wraps a Graph and delegates GetCallers/GetCallees back to it,
// simulating the cycle that caused a stack overflow in production (x-ray focus.Router).
type delegatingGraph struct {
	Graph
	delegate Graph
}

func (d *delegatingGraph) GetCallers(token string) ([]*Node, error) {
	return d.delegate.GetCallers(token)
}

func (d *delegatingGraph) GetCallees(id string) ([]*Node, error) {
	return d.delegate.GetCallees(id)
}

// TestCompositeGraph_GetCallers_CycleProtection verifies that mounting a graph
// which delegates GetCallers back to the CompositeGraph does not stack-overflow.
func TestCompositeGraph_GetCallers_CycleProtection(t *testing.T) {
	c := NewCompositeGraph()
	store := testStore("zone", nil, map[string][]byte{"zone/desc": []byte("hi")})
	_ = c.Mount("browser", store)

	// Mount a delegating graph that points back to the composite — this is the cycle.
	_ = c.Mount("focus", &delegatingGraph{Graph: NewMemoryStore(), delegate: c})

	// This would stack-overflow without the recursion guard.
	callers, err := c.GetCallers("zone")
	if err != nil {
		t.Fatalf("GetCallers should not error: %v", err)
	}
	// Should return results from browser (the real store), not crash.
	_ = callers

	callees, err := c.GetCallees("/browser/zone")
	if err != nil {
		t.Fatalf("GetCallees should not error: %v", err)
	}
	_ = callees
}

func TestCompositeGraph_LeadingSlash(t *testing.T) {
	s := testStore("zone", nil, map[string][]byte{
		"zone/desc": []byte("hello"),
	})
	c := NewCompositeGraph()
	_ = c.Mount("browser", s)

	// Paths with leading slash should work
	buf := make([]byte, 100)
	n, err := c.ReadContent("/browser/zone/desc", buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", buf[:n])
	}

	node, err := c.GetNode("/browser")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Mode.IsDir() {
		t.Fatal("mount point should be a directory")
	}
}
