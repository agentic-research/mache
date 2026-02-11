package graph

import (
	"io/fs"
	"testing"
)

func TestMemoryStore_AddRootAndGetNode(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:   "vulns",
		Mode: fs.ModeDir,
		Children: []string{
			"vulns/CVE-2024-1234",
		},
	})

	node, err := store.GetNode("vulns")
	if err != nil {
		t.Fatalf("GetNode(vulns) returned error: %v", err)
	}
	if !node.Mode.IsDir() {
		t.Error("vulns should be a directory")
	}
	if len(node.Children) != 1 {
		t.Errorf("vulns children = %d, want 1", len(node.Children))
	}
}

func TestMemoryStore_AddNodeFileWithData(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{
		ID:   "vulns/CVE-2024-1234/severity",
		Mode: 0, // regular file
		Data: []byte("CRITICAL\n"),
	})

	node, err := store.GetNode("vulns/CVE-2024-1234/severity")
	if err != nil {
		t.Fatalf("GetNode returned error: %v", err)
	}
	if node.Mode.IsDir() {
		t.Error("severity should be a regular file, not a directory")
	}
	if string(node.Data) != "CRITICAL\n" {
		t.Errorf("Data = %q, want %q", node.Data, "CRITICAL\n")
	}
}

func TestMemoryStore_GetNodeNormalizesLeadingSlash(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{ID: "foo", Mode: fs.ModeDir})

	node, err := store.GetNode("/foo")
	if err != nil {
		t.Fatalf("GetNode(/foo) should resolve to foo: %v", err)
	}
	if node.ID != "foo" {
		t.Errorf("ID = %q, want %q", node.ID, "foo")
	}
}

func TestMemoryStore_ListChildrenRoot(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "vulns", Mode: fs.ModeDir})
	store.AddRoot(&Node{ID: "advisories", Mode: fs.ModeDir})

	roots, err := store.ListChildren("/")
	if err != nil {
		t.Fatalf("ListChildren(/) returned error: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots = %d, want 2", len(roots))
	}
}

func TestMemoryStore_ListChildrenNode(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "vulns",
		Mode:     fs.ModeDir,
		Children: []string{"vulns/CVE-1", "vulns/CVE-2"},
	})

	children, err := store.ListChildren("vulns")
	if err != nil {
		t.Fatalf("ListChildren(vulns) returned error: %v", err)
	}
	if len(children) != 2 {
		t.Errorf("children = %d, want 2", len(children))
	}
}

func TestMemoryStore_GetNodeNotFound(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.GetNode("nonexistent")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_AddRootDeduplicates(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "vulns", Mode: fs.ModeDir})
	store.AddRoot(&Node{ID: "vulns", Mode: fs.ModeDir})

	roots, err := store.ListChildren("/")
	if err != nil {
		t.Fatalf("ListChildren(/) returned error: %v", err)
	}
	if len(roots) != 1 {
		t.Errorf("roots = %d, want 1 (deduped)", len(roots))
	}
}

func TestMemoryStore_FileNodeHasNoChildren(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{
		ID:   "vulns/report.pdf",
		Mode: 0, // regular file
		Data: []byte("%PDF-1.4..."),
	})

	node, err := store.GetNode("vulns/report.pdf")
	if err != nil {
		t.Fatalf("GetNode returned error: %v", err)
	}
	if node.Mode.IsDir() {
		t.Error("report.pdf should be a file")
	}
	if len(node.Children) != 0 {
		t.Error("file node should have no children")
	}
	if len(node.Data) == 0 {
		t.Error("file node should have data")
	}
}

func TestMemoryStore_DirAndFileCoexist(t *testing.T) {
	store := NewMemoryStore()

	// A directory node
	store.AddRoot(&Node{
		ID:       "vulns",
		Mode:     fs.ModeDir,
		Children: []string{"vulns/CVE-1", "vulns/report.pdf"},
	})

	// A child directory
	store.AddNode(&Node{
		ID:       "vulns/CVE-1",
		Mode:     fs.ModeDir,
		Children: []string{"vulns/CVE-1/severity"},
	})

	// A file as a sibling to a directory
	store.AddNode(&Node{
		ID:   "vulns/report.pdf",
		Mode: 0,
		Data: []byte("report content"),
	})

	// A file as a child of CVE-1
	store.AddNode(&Node{
		ID:   "vulns/CVE-1/severity",
		Mode: 0,
		Data: []byte("CRITICAL\n"),
	})

	// Verify the directory
	dirNode, err := store.GetNode("vulns/CVE-1")
	if err != nil {
		t.Fatal(err)
	}
	if !dirNode.Mode.IsDir() {
		t.Error("CVE-1 should be a directory")
	}

	// Verify the sibling file
	fileNode, err := store.GetNode("vulns/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if fileNode.Mode.IsDir() {
		t.Error("report.pdf should be a file")
	}

	// Verify the nested file
	sevNode, err := store.GetNode("vulns/CVE-1/severity")
	if err != nil {
		t.Fatal(err)
	}
	if sevNode.Mode.IsDir() {
		t.Error("severity should be a file")
	}
	if string(sevNode.Data) != "CRITICAL\n" {
		t.Errorf("Data = %q, want %q", sevNode.Data, "CRITICAL\n")
	}
}
