package graph

import (
	"io/fs"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// Roaring file index + ShiftOrigins tests
// ---------------------------------------------------------------------------

func TestMemoryStore_FileIndex_AddNode(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:   "pkg/main/source.go/FuncA",
		Mode: 0,
		Data: []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 0,
			EndByte:   12,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/main/source.go/FuncB",
		Mode: 0,
		Data: []byte("func B() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 13,
			EndByte:   25,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/util/helper.go/FuncC",
		Mode: 0,
		Data: []byte("func C() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/helper.go",
			StartByte: 0,
			EndByte:   12,
		},
	})

	// Verify bitmap index populated
	store.mu.RLock()
	defer store.mu.RUnlock()

	bm, ok := store.fileToNodes["/src/main.go"]
	require.True(t, ok, "bitmap should exist for /src/main.go")
	assert.Equal(t, uint64(2), bm.GetCardinality(), "should have 2 nodes for main.go")

	bm2, ok := store.fileToNodes["/src/helper.go"]
	require.True(t, ok, "bitmap should exist for /src/helper.go")
	assert.Equal(t, uint64(1), bm2.GetCardinality(), "should have 1 node for helper.go")
}

func TestMemoryStore_FileIndex_NoOrigin(t *testing.T) {
	store := NewMemoryStore()

	// Nodes without Origin should NOT appear in file index
	store.AddNode(&Node{
		ID:   "virtual/dir",
		Mode: fs.ModeDir,
	})

	store.mu.RLock()
	defer store.mu.RUnlock()

	assert.Empty(t, store.fileToNodes, "no bitmap should be created for nodes without Origin")
}

func TestMemoryStore_DeleteFileNodes_UsesBitmap(t *testing.T) {
	store := NewMemoryStore()

	// Parent dir
	store.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/FuncA", "pkg/FuncB", "pkg/FuncC"},
	})

	store.AddNode(&Node{
		ID:   "pkg/FuncA",
		Mode: 0,
		Data: []byte("func A"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 0,
			EndByte:   6,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/FuncB",
		Mode: 0,
		Data: []byte("func B"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 7,
			EndByte:   13,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/FuncC",
		Mode: 0,
		Data: []byte("func C"),
		Origin: &SourceOrigin{
			FilePath:  "/src/other.go",
			StartByte: 0,
			EndByte:   6,
		},
	})

	// Delete all nodes from main.go
	store.DeleteFileNodes("/src/main.go")

	// FuncA and FuncB should be gone
	_, err := store.GetNode("pkg/FuncA")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetNode("pkg/FuncB")
	assert.ErrorIs(t, err, ErrNotFound)

	// FuncC should remain
	n, err := store.GetNode("pkg/FuncC")
	require.NoError(t, err)
	assert.Equal(t, "func C", string(n.Data))

	// Parent's children should be cleaned up
	parent, err := store.GetNode("pkg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/FuncC"}, parent.Children)
}

func TestMemoryStore_ShiftOrigins_PositiveDelta(t *testing.T) {
	store := NewMemoryStore()

	// Simulate two functions in one file
	store.AddNode(&Node{
		ID:   "pkg/FuncA",
		Mode: 0,
		Data: []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 0,
			EndByte:   12,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/FuncB",
		Mode: 0,
		Data: []byte("func B() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 13,
			EndByte:   25,
		},
	})

	// Splice FuncA: old was 12 bytes, new is 20 bytes → delta = +8
	// afterByte = FuncA.EndByte = 12
	store.ShiftOrigins("/src/main.go", 12, 8)

	// FuncA should be untouched (starts before afterByte)
	nodeA, _ := store.GetNode("pkg/FuncA")
	assert.Equal(t, uint32(0), nodeA.Origin.StartByte)
	assert.Equal(t, uint32(12), nodeA.Origin.EndByte)

	// FuncB should be shifted by +8
	nodeB, _ := store.GetNode("pkg/FuncB")
	assert.Equal(t, uint32(21), nodeB.Origin.StartByte) // 13 + 8
	assert.Equal(t, uint32(33), nodeB.Origin.EndByte)   // 25 + 8
}

func TestMemoryStore_ShiftOrigins_NegativeDelta(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:   "pkg/FuncA",
		Mode: 0,
		Data: []byte("func A() { /* long */ }"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 0,
			EndByte:   23,
		},
	})
	store.AddNode(&Node{
		ID:   "pkg/FuncB",
		Mode: 0,
		Data: []byte("func B() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 24,
			EndByte:   36,
		},
	})

	// Splice FuncA: old was 23 bytes, new is 12 bytes → delta = -11
	store.ShiftOrigins("/src/main.go", 23, -11)

	nodeB, _ := store.GetNode("pkg/FuncB")
	assert.Equal(t, uint32(13), nodeB.Origin.StartByte) // 24 - 11
	assert.Equal(t, uint32(25), nodeB.Origin.EndByte)   // 36 - 11
}

// ---------------------------------------------------------------------------
// UpdateNodeContent / UpdateNodeContext tests
// ---------------------------------------------------------------------------

func TestUpdateNodeContent_PreservesChildren(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:       "pkg/main",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/main/FuncA", "pkg/main/FuncB"},
		Context:  []byte("package main\n"),
	})
	store.AddNode(&Node{
		ID:   "pkg/main/FuncA",
		Mode: 0,
		Data: []byte("func A() {}"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 20,
			EndByte:   32,
		},
	})

	// Update FuncA content
	newData := []byte("func A() { return 42 }")
	newOrigin := &SourceOrigin{
		FilePath:  "/src/main.go",
		StartByte: 20,
		EndByte:   20 + uint32(len(newData)),
	}
	err := store.UpdateNodeContent("pkg/main/FuncA", newData, newOrigin, time.Now())
	require.NoError(t, err)

	// Verify content updated
	node, err := store.GetNode("pkg/main/FuncA")
	require.NoError(t, err)
	assert.Equal(t, newData, node.Data)
	assert.Equal(t, newOrigin, node.Origin)

	// Verify parent's Children are untouched
	parent, err := store.GetNode("pkg/main")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/main/FuncA", "pkg/main/FuncB"}, parent.Children)
	assert.Equal(t, []byte("package main\n"), parent.Context)
}

func TestUpdateNodeContent_ClearsDraft(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:        "pkg/main/FuncA",
		Mode:      0,
		Data:      []byte("func A() {}"),
		DraftData: []byte("func A() { broken"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 0,
			EndByte:   12,
		},
	})

	err := store.UpdateNodeContent("pkg/main/FuncA", []byte("func A() { fixed }"), nil, time.Now())
	require.NoError(t, err)

	node, _ := store.GetNode("pkg/main/FuncA")
	assert.Nil(t, node.DraftData, "DraftData should be cleared after successful update")
	assert.Equal(t, "func A() { fixed }", string(node.Data))
}

func TestUpdateNodeContent_NotFound(t *testing.T) {
	store := NewMemoryStore()

	err := store.UpdateNodeContent("nonexistent", []byte("data"), nil, time.Now())
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestUpdateNodeContext_Basic(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:      "pkg/main",
		Mode:    fs.ModeDir,
		Context: []byte("package main\n"),
	})

	err := store.UpdateNodeContext("pkg/main", []byte("package main\n\nimport \"fmt\"\n"))
	require.NoError(t, err)

	node, _ := store.GetNode("pkg/main")
	assert.Equal(t, "package main\n\nimport \"fmt\"\n", string(node.Context))
}

func TestUpdateNodeContext_NotFound(t *testing.T) {
	store := NewMemoryStore()

	err := store.UpdateNodeContext("nonexistent", []byte("ctx"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryStore_ShiftOrigins_NoOpDifferentFile(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:   "pkg/FuncX",
		Mode: 0,
		Data: []byte("func X"),
		Origin: &SourceOrigin{
			FilePath:  "/src/other.go",
			StartByte: 10,
			EndByte:   16,
		},
	})

	// Shift on a different file should be a no-op
	store.ShiftOrigins("/src/main.go", 0, 100)

	nodeX, _ := store.GetNode("pkg/FuncX")
	assert.Equal(t, uint32(10), nodeX.Origin.StartByte)
	assert.Equal(t, uint32(16), nodeX.Origin.EndByte)
}
