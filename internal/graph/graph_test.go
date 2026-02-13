package graph

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// ---------------------------------------------------------------------------
// MemoryStore refs / SQL query tests
// ---------------------------------------------------------------------------

func TestMemoryStore_FlushRefs_QueryRefs(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()

	// Add refs: 3 tokens across 2 files
	require.NoError(t, store.AddRef("Println", "pkg/main/source.go"))
	require.NoError(t, store.AddRef("Println", "pkg/util/helper.go"))
	require.NoError(t, store.AddRef("Sprintf", "pkg/main/source.go"))
	require.NoError(t, store.AddRef("ErrorNew", "pkg/util/helper.go"))

	require.NoError(t, store.FlushRefs())

	// Query via mache_refs vtab for a specific token
	rows, err := store.QueryRefs("SELECT path FROM mache_refs WHERE token = ?", "Println")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		paths = append(paths, p)
	}
	require.NoError(t, rows.Err())

	assert.Len(t, paths, 2)
	assert.Contains(t, paths, "pkg/main/source.go")
	assert.Contains(t, paths, "pkg/util/helper.go")
}

func TestMemoryStore_VTab_LIKE(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()

	require.NoError(t, store.AddRef("MyComponent", "ui/app.go"))
	require.NoError(t, store.AddRef("MyHelper", "ui/helper.go"))
	require.NoError(t, store.AddRef("OtherThing", "pkg/other.go"))

	require.NoError(t, store.FlushRefs())

	rows, err := store.QueryRefs("SELECT path FROM mache_refs WHERE token LIKE ?", "My%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		paths = append(paths, p)
	}
	require.NoError(t, rows.Err())

	assert.Len(t, paths, 2)
	assert.Contains(t, paths, "ui/app.go")
	assert.Contains(t, paths, "ui/helper.go")
}

func TestMemoryStore_FlushRefs_Idempotent(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()

	require.NoError(t, store.AddRef("Foo", "a.go"))
	require.NoError(t, store.FlushRefs())
	require.NoError(t, store.FlushRefs()) // second call is a no-op

	// Data should still be intact
	rows, err := store.QueryRefs("SELECT path FROM mache_refs WHERE token = ?", "Foo")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		paths = append(paths, p)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"a.go"}, paths)
}

func TestMemoryStore_FlushRefs_Empty(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()

	// FlushRefs with no refs added — should succeed without error
	require.NoError(t, store.FlushRefs())
}

func TestMemoryStore_QueryRefs_BeforeInit(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.QueryRefs("SELECT 1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "refsDB not initialized")
}
