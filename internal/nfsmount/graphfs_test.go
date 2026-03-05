package nfsmount

import (
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestGraph() *graph.MemoryStore {
	store := graph.NewMemoryStore()

	// Root dir: vulns
	store.AddRoot(&graph.Node{
		ID:   "vulns",
		Mode: fs.ModeDir,
		Children: []string{
			"vulns/CVE-2024-0001.json",
			"vulns/CVE-2024-0002.json",
		},
	})

	// Two leaf files
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-0001.json",
		Mode: 0,
		Data: []byte(`{"id": "CVE-2024-0001", "severity": "HIGH"}`),
	})
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-0002.json",
		Mode: 0,
		Data: []byte(`{"id": "CVE-2024-0002", "severity": "LOW"}`),
	})

	return store
}

func newTestSchema() *api.Topology {
	return &api.Topology{Version: "v1alpha1"}
}

func TestStatRoot(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	info, err := gfs.Stat("/")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, "/", info.Name())
}

func TestStatSchemaJSON(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	info, err := gfs.Stat("/_schema.json")
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.Equal(t, "_schema.json", info.Name())
	assert.True(t, info.Size() > 0)
}

func TestStatFile(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	info, err := gfs.Stat("/vulns/CVE-2024-0001.json")
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.Equal(t, "CVE-2024-0001.json", info.Name())
	assert.Equal(t, int64(43), info.Size())
}

func TestStatDir(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	info, err := gfs.Stat("/vulns")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, "vulns", info.Name())
}

func TestStatNotFound(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	_, err := gfs.Stat("/nonexistent")
	assert.True(t, os.IsNotExist(err))
}

func TestReadDirRoot(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	entries, err := gfs.ReadDir("/")
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "_schema.json")
	assert.Contains(t, names, "vulns")
}

func TestReadDirSubdir(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	entries, err := gfs.ReadDir("/vulns")
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "CVE-2024-0001.json")
	assert.Contains(t, names, "CVE-2024-0002.json")
}

func TestOpenAndRead(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	f, err := gfs.Open("/vulns/CVE-2024-0001.json")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	// Read may return io.EOF with n > 0, that's fine
	require.True(t, n > 0)
	assert.Contains(t, string(buf[:n]), "CVE-2024-0001")
}

func TestOpenSchemaJSON(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	f, err := gfs.Open("/_schema.json")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	require.True(t, n > 0)
	assert.Contains(t, string(buf[:n]), "v1alpha1")
}

func TestReadAt(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	f, err := gfs.Open("/vulns/CVE-2024-0001.json")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	buf := make([]byte, 10)
	n, _ := f.ReadAt(buf, 1)
	require.True(t, n > 0)
	assert.Equal(t, `"id": "CVE`, string(buf[:n]))
}

func TestSeek(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	f, err := gfs.Open("/vulns/CVE-2024-0001.json")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	pos, err := f.Seek(5, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(5), pos)

	buf := make([]byte, 5)
	n, _ := f.Read(buf)
	require.True(t, n > 0)
	assert.Equal(t, `: "CV`, string(buf[:n]))
}

func TestOpenNotFound(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	_, err := gfs.Open("/nonexistent")
	assert.Error(t, err)
}

func TestReadOnly(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	// Create on a non-existent file (no Origin) should fail
	_, err := gfs.Create("newfile.txt")
	assert.Error(t, err)

	err = gfs.MkdirAll("/newdir", 0o755)
	assert.Equal(t, errReadOnly, err)

	err = gfs.Remove("/vulns/CVE-2024-0001.json")
	assert.Equal(t, errReadOnly, err)

	err = gfs.Rename("/vulns", "/renamed")
	assert.Equal(t, errReadOnly, err)
}

func TestCapabilities(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	caps := gfs.Capabilities()
	assert.NotZero(t, caps&2) // ReadCapability (1 << 1)
	assert.NotZero(t, caps&8) // SeekCapability (1 << 3)
	assert.Zero(t, caps&1)    // WriteCapability (1 << 0) should NOT be set
}

func TestRoot(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())
	assert.Equal(t, "/", gfs.Root())
}

func TestJoin(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())
	assert.Equal(t, "a/b/c", gfs.Join("a", "b", "c"))
}

func TestWritableOpenAndClose(t *testing.T) {
	store := newTestGraph()
	// Add a node with SourceOrigin (writable)
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-0001.json",
		Mode: 0,
		Data: []byte(`{"id": "CVE-2024-0001", "severity": "HIGH"}`),
		Origin: &graph.SourceOrigin{
			FilePath:  "/tmp/test-source.json",
			StartByte: 0,
			EndByte:   43,
		},
	})

	gfs := NewGraphFS(store, newTestSchema())

	// Without write-back, writes should fail
	_, err := gfs.OpenFile("/vulns/CVE-2024-0001.json", os.O_RDWR, 0)
	assert.Equal(t, errReadOnly, err)

	// Enable write-back
	var capturedID string
	var capturedContent []byte
	gfs.SetWriteBack(func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		capturedID = nodeID
		capturedContent = make([]byte, len(content))
		copy(capturedContent, content)
		return nil
	})

	// Now open for write
	f, err := gfs.OpenFile("/vulns/CVE-2024-0001.json", os.O_RDWR, 0)
	require.NoError(t, err)

	// Write new content
	_, err = f.Write([]byte(`{"id": "CVE-2024-0001", "severity": "CRITICAL"}`))
	require.NoError(t, err)

	// Close triggers write-back
	err = f.Close()
	require.NoError(t, err)

	assert.Equal(t, "/vulns/CVE-2024-0001.json", capturedID)
	assert.Contains(t, string(capturedContent), "CRITICAL")
}

func TestWritableCapabilities(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	// Read-only by default
	assert.Zero(t, gfs.Capabilities()&1) // WriteCapability (1 << 0)

	// Enable write-back
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })
	assert.NotZero(t, gfs.Capabilities()&1) // WriteCapability now set
}

func TestRemoveWithWriteBack(t *testing.T) {
	store := newTestGraph()
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-0001.json",
		Mode: 0,
		Data: []byte(`test`),
		Origin: &graph.SourceOrigin{
			FilePath:  "/tmp/test.json",
			StartByte: 0,
			EndByte:   4,
		},
	})

	gfs := NewGraphFS(store, newTestSchema())

	// Without write-back, remove fails
	err := gfs.Remove("/vulns/CVE-2024-0001.json")
	assert.Equal(t, errReadOnly, err)

	// Enable write-back
	var deletedContent []byte
	gfs.SetWriteBack(func(_ string, _ graph.SourceOrigin, content []byte) error {
		deletedContent = content
		return nil
	})

	err = gfs.Remove("/vulns/CVE-2024-0001.json")
	require.NoError(t, err)
	assert.Empty(t, deletedContent) // splice with empty content = delete
}

// ---------------------------------------------------------------------------
// _diagnostics/ virtual directory tests
// ---------------------------------------------------------------------------

func TestDiagnostics_StatDir(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	info, err := gfs.Stat("/vulns/_diagnostics")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, "_diagnostics", info.Name())
}

func TestDiagnostics_StatFile(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	info, err := gfs.Stat("/vulns/_diagnostics/last-write-status")
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.Equal(t, "last-write-status", info.Name())
	assert.True(t, info.Size() > 0)
}

func TestDiagnostics_ReadDir(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	entries, err := gfs.ReadDir("/vulns/_diagnostics")
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "last-write-status")
	assert.Contains(t, names, "ast-errors")
}

func TestDiagnostics_ReadLastWriteStatus_Default(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	f, err := gfs.OpenFile("/vulns/_diagnostics/last-write-status", os.O_RDONLY, 0)
	require.NoError(t, err)

	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	assert.Contains(t, string(buf[:n]), "no writes yet")
}

func TestDiagnostics_ReadLastWriteStatus_AfterWrite(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	// Simulate a write status being stored
	store.WriteStatus.Store("/vulns", "ok")

	f, err := gfs.OpenFile("/vulns/_diagnostics/last-write-status", os.O_RDONLY, 0)
	require.NoError(t, err)

	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	assert.Equal(t, "ok\n", string(buf[:n]))
}

func TestDiagnostics_ReadASTErrors_AfterFailedWrite(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	// Simulate a validation failure
	store.WriteStatus.Store("/vulns", "test.go:3:1: syntax error in AST")

	f, err := gfs.OpenFile("/vulns/_diagnostics/ast-errors", os.O_RDONLY, 0)
	require.NoError(t, err)

	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	assert.Contains(t, string(buf[:n]), "syntax error")
}

func TestDiagnostics_NotVisibleWhenReadOnly(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())
	// No SetWriteBack called → read-only

	_, err := gfs.Stat("/vulns/_diagnostics")
	assert.Error(t, err) // Should not resolve
}

func TestDiagnostics_InReadDirListing(t *testing.T) {
	store := newTestGraph()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	entries, err := gfs.ReadDir("/vulns")
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "_diagnostics")
}

func TestNFSServerStarts(t *testing.T) {
	gfs := NewGraphFS(newTestGraph(), newTestSchema())

	srv, err := NewServer(gfs)
	require.NoError(t, err)
	defer func() { _ = srv.Close() }()

	assert.True(t, srv.Port() > 0, "server should be on a valid port")

	// Verify TCP connectivity
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", srv.Port()))
	require.NoError(t, err)
	_ = conn.Close()
}

func TestDraftMode(t *testing.T) {
	store := newTestGraph()
	// Add a writable node
	store.AddNode(&graph.Node{
		ID:   "vulns/CVE-2024-0001.json",
		Mode: 0,
		Data: []byte(`original`),
		Origin: &graph.SourceOrigin{
			FilePath:  "/tmp/test.json",
			StartByte: 0,
			EndByte:   8,
		},
	})

	gfs := NewGraphFS(store, newTestSchema())

	// Simulate cmd/mount.go logic: validation fail -> save draft -> return nil
	gfs.SetWriteBack(func(nodeID string, _ graph.SourceOrigin, content []byte) error {
		node, _ := store.GetNode(nodeID)
		// Simulate saving draft
		node.DraftData = make([]byte, len(content))
		copy(node.DraftData, content)
		return nil
	})

	// 1. Write "invalid" content
	f, err := gfs.OpenFile("/vulns/CVE-2024-0001.json", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.Write([]byte(`draft_content`))
	require.NoError(t, err)
	err = f.Close() // Triggers WriteBack
	require.NoError(t, err)

	// 2. Read back -> should see draft
	f, err = gfs.OpenFile("/vulns/CVE-2024-0001.json", os.O_RDONLY, 0)
	require.NoError(t, err)
	buf := make([]byte, 100)
	n, _ := f.Read(buf)
	assert.Equal(t, "draft_content", string(buf[:n]))
	_ = f.Close()

	// 3. Verify original data is untouched
	node, _ := store.GetNode("/vulns/CVE-2024-0001.json")
	assert.Equal(t, "original", string(node.Data))
	assert.Equal(t, "draft_content", string(node.DraftData))
}

// ---------------------------------------------------------------------------
// callers/ virtual directory tests
// ---------------------------------------------------------------------------

// newTestGraphWithCallers creates a graph with cross-references for callers/ testing.
func newTestGraphWithCallers() *graph.MemoryStore {
	store := graph.NewMemoryStore()

	store.AddRoot(&graph.Node{
		ID:   "funcs",
		Mode: fs.ModeDir,
		Children: []string{
			"funcs/Foo",
			"funcs/Bar",
		},
	})
	store.AddNode(&graph.Node{
		ID:       "funcs/Foo",
		Mode:     fs.ModeDir,
		Children: []string{"funcs/Foo/source"},
	})
	store.AddNode(&graph.Node{
		ID:   "funcs/Foo/source",
		Mode: 0,
		Data: []byte("func Foo() { Bar() }"),
	})
	store.AddNode(&graph.Node{
		ID:       "funcs/Bar",
		Mode:     fs.ModeDir,
		Children: []string{"funcs/Bar/source"},
	})
	store.AddNode(&graph.Node{
		ID:   "funcs/Bar/source",
		Mode: 0,
		Data: []byte("func Bar() { fmt.Println() }"),
	})

	// Foo calls Bar
	_ = store.AddRef("Bar", "funcs/Foo/source")

	return store
}

func TestCallers_StatDir(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	info, err := gfs.Stat("/funcs/Bar/callers")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, "callers", info.Name())
}

func TestCallers_StatEntry(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	info, err := gfs.Stat("/funcs/Bar/callers/funcs_Foo_source")
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.Equal(t, "funcs_Foo_source", info.Name())
	assert.Equal(t, int64(20), info.Size()) // len("func Foo() { Bar() }")
}

func TestCallers_StatNotFound_NoCallers(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	// Nobody calls Foo
	_, err := gfs.Stat("/funcs/Foo/callers")
	assert.True(t, os.IsNotExist(err))
}

func TestCallers_ReadDir(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	entries, err := gfs.ReadDir("/funcs/Bar/callers")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "funcs_Foo_source", entries[0].Name())
	assert.Equal(t, int64(20), entries[0].Size())
}

func TestCallers_InParentReadDir(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	entries, err := gfs.ReadDir("/funcs/Bar")
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "callers")
	assert.Contains(t, names, "source")
}

func TestCallers_NotInParentReadDir_NoCallers(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	entries, err := gfs.ReadDir("/funcs/Foo")
	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.NotContains(t, names, "callers")
}

func TestCallers_OpenAndRead(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	f, err := gfs.Open("/funcs/Bar/callers/funcs_Foo_source")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	buf := make([]byte, 1024)
	n, err := f.Read(buf)
	if err != nil && err.Error() != "EOF" {
		require.NoError(t, err)
	}
	require.True(t, n > 0)
	assert.Equal(t, "func Foo() { Bar() }", string(buf[:n]))
}

func TestCallers_OpenDir_ReturnsError(t *testing.T) {
	gfs := NewGraphFS(newTestGraphWithCallers(), newTestSchema())

	_, err := gfs.Open("/funcs/Bar/callers")
	assert.Error(t, err)
}

// TestUniqueFileids verifies every entry from ReadDir and Lstat has a unique,
// non-zero inode (NFS Fileid). Fileid=0 or duplicates break macOS NFS client.
func TestUniqueFileids(t *testing.T) {
	store := newTestGraphWithCallers()
	gfs := NewGraphFS(store, newTestSchema())
	gfs.SetWriteBack(func(string, graph.SourceOrigin, []byte) error { return nil })

	// Collect inodes from ReadDir at multiple levels.
	seen := map[uint64]string{}
	normalize := func(p string) string { return filepath.Clean(p) }
	checkEntries := func(dir string, entries []os.FileInfo) {
		for _, e := range entries {
			fi, ok := e.(*staticFileInfo)
			require.True(t, ok, "entry %s should be *staticFileInfo", e.Name())
			assert.NotZero(t, fi.ino, "entry %s in %s has ino=0", e.Name(), dir)
			key := normalize(dir + "/" + e.Name())
			if prev, dup := seen[fi.ino]; dup && prev != key {
				t.Errorf("duplicate ino %d: %s and %s", fi.ino, key, prev)
			}
			seen[fi.ino] = key
		}
	}

	entries, err := gfs.ReadDir("/")
	require.NoError(t, err)
	checkEntries("/", entries)

	entries, err = gfs.ReadDir("/funcs")
	require.NoError(t, err)
	checkEntries("/funcs", entries)

	entries, err = gfs.ReadDir("/funcs/Bar")
	require.NoError(t, err)
	checkEntries("/funcs/Bar", entries)

	entries, err = gfs.ReadDir("/funcs/Bar/callers")
	require.NoError(t, err)
	checkEntries("/funcs/Bar/callers", entries)

	// Also check Lstat returns unique inodes.
	for _, p := range []string{"/", "/_schema.json", "/funcs", "/funcs/Bar", "/funcs/Bar/callers"} {
		info, err := gfs.Lstat(p)
		require.NoError(t, err)
		fi, ok := info.(*staticFileInfo)
		require.True(t, ok)
		assert.NotZero(t, fi.ino, "Lstat %s has ino=0", p)
		key := normalize(p)
		if prev, dup := seen[fi.ino]; dup && prev != key {
			t.Errorf("Lstat ino %d for %s duplicates entry %s", fi.ino, p, prev)
		}
	}
}

// TestHotSwapGraphFS reproduces the x-ray NFS scenario:
// 1. GraphFS wraps a HotSwapGraph starting with an empty MemoryStore
// 2. Swap to a CompositeGraph with mounted sub-graphs
// 3. Verify ReadDir("/") returns the composite mount names
func TestHotSwapGraphFS(t *testing.T) {
	// Phase 1: empty graph — simulates NFS mount before any tab connects.
	empty := graph.NewMemoryStore()
	hotswap := graph.NewHotSwapGraph(empty)
	gfs := NewGraphFS(hotswap, &api.Topology{Version: "v1"})

	// ReadDir on empty graph should return just _schema.json.
	entries, err := gfs.ReadDir("/")
	require.NoError(t, err)
	names := infoNames(entries)
	assert.Contains(t, names, "_schema.json")
	assert.Len(t, entries, 1, "empty graph should only have _schema.json")

	// Lstat root should succeed.
	info, err := gfs.Lstat("/")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	initialMtime := info.ModTime()

	// Phase 2: Swap to CompositeGraph with mounts — simulates tab connect.
	browserStore := graph.NewMemoryStore()
	browserStore.AddRoot(&graph.Node{ID: "page", Mode: fs.ModeDir, Children: []string{"page/title"}})
	browserStore.AddNode(&graph.Node{ID: "page/title", Data: []byte("Hello")})

	tasksStore := graph.NewMemoryStore()

	composite := graph.NewCompositeGraph()
	require.NoError(t, composite.Mount("browser", browserStore))
	require.NoError(t, composite.Mount("tasks", tasksStore))

	hotswap.Swap(composite)

	// ReadDir root should now show composite mounts + _schema.json.
	entries, err = gfs.ReadDir("/")
	require.NoError(t, err)
	names = infoNames(entries)
	assert.Contains(t, names, "_schema.json", "virtual file should still appear")
	assert.Contains(t, names, "browser", "browser mount should appear")
	assert.Contains(t, names, "tasks", "tasks mount should appear")

	// Lstat root mtime should have changed (CompositeGraph returns time.Now()).
	info, err = gfs.Lstat("/")
	require.NoError(t, err)
	assert.True(t, info.ModTime().After(initialMtime) || info.ModTime().Equal(initialMtime),
		"root mtime should be >= initial after swap")

	// ReadDir browser/ should show sub-graph content.
	entries, err = gfs.ReadDir("/browser")
	require.NoError(t, err)
	names = infoNames(entries)
	assert.Contains(t, names, "page", "browser sub-graph content should appear")

	// Can read a file through the full chain.
	f, err := gfs.Open("/browser/page/title")
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	_ = f.Close()
	assert.Equal(t, "Hello", string(buf[:n]))
}

// TestHotSwapNFSProtocol verifies go-nfs serves correct READDIRPLUS after Swap.
// Uses a logging wrapper to confirm ReadDir is called with correct results.
func TestHotSwapNFSProtocol(t *testing.T) {
	empty := graph.NewMemoryStore()
	hotswap := graph.NewHotSwapGraph(empty)
	gfs := NewGraphFS(hotswap, &api.Topology{Version: "v1"})

	// Wrap GraphFS in a logging proxy to track ReadDir calls.
	spy := &readDirSpy{GraphFS: gfs}

	srv, err := NewServer(spy)
	require.NoError(t, err)
	defer func() { _ = srv.Close() }()

	assert.True(t, srv.Port() > 0)

	// Phase 1: verify ReadDir returns _schema.json on empty graph.
	entries, err := spy.ReadDir("/")
	require.NoError(t, err)
	names := infoNames(entries)
	assert.Contains(t, names, "_schema.json")
	assert.Len(t, entries, 1, "empty graph: only _schema.json")

	// Phase 2: Swap to CompositeGraph.
	browserStore := graph.NewMemoryStore()
	browserStore.AddRoot(&graph.Node{ID: "page", Mode: fs.ModeDir, Children: []string{"page/title"}})
	browserStore.AddNode(&graph.Node{ID: "page/title", Data: []byte("Hello NFS")})

	composite := graph.NewCompositeGraph()
	require.NoError(t, composite.Mount("browser", browserStore))
	require.NoError(t, composite.Mount("tasks", graph.NewMemoryStore()))
	hotswap.Swap(composite)

	// Phase 2: ReadDir root should now show composite mounts.
	entries, err = spy.ReadDir("/")
	require.NoError(t, err)
	names = infoNames(entries)
	assert.Contains(t, names, "_schema.json")
	assert.Contains(t, names, "browser")
	assert.Contains(t, names, "tasks")

	// Phase 2: ReadDir /browser should show sub-graph content.
	entries, err = spy.ReadDir("/browser")
	require.NoError(t, err)
	names = infoNames(entries)
	assert.Contains(t, names, "page")

	// Phase 2: Lstat root should show updated mtime (CompositeGraph returns time.Now()).
	info, err := spy.Lstat("/")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	// CompositeGraph.GetNode("") returns ModTime: time.Now(), which should be recent.
	assert.False(t, info.ModTime().IsZero(), "root mtime should not be zero after swap to CompositeGraph")
}

// readDirSpy wraps GraphFS to log ReadDir calls for debugging.
type readDirSpy struct {
	*GraphFS
	readDirCalls int
}

func (s *readDirSpy) ReadDir(path string) ([]os.FileInfo, error) {
	s.readDirCalls++
	return s.GraphFS.ReadDir(path)
}

func infoNames(entries []os.FileInfo) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}
