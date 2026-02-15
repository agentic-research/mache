package nfsmount

import (
	"fmt"
	"io/fs"
	"net"
	"os"
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
