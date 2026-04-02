package graph

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Graph interface test suite
//
// Tests the Graph contract, not any specific implementation. Every backend
// provides a factory that builds the same canonical graph shape. The suite
// verifies identical behavior across all implementations.
//
// To add a new Graph implementation: add a factory and a TestXxx_GraphSuite
// entry at the bottom of this file. The suite tells you what's broken.
// ---------------------------------------------------------------------------

// Canonical test graph shape (every factory must build this):
//
//	root = "pkg"
//	pkg/                    (dir)
//	├── auth/               (dir)
//	│   ├── source          (file, 20 bytes: "func Validate() {}\n")
//	│   └── doc             (file, 17 bytes: "// auth validate\n")
//	├── main/               (dir)
//	│   └── source          (file, 13 bytes: "package main\n")
//	└── empty/              (dir, no children)

var (
	suiteModTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	suiteAuthSource = []byte("func Validate() {}\n")
	suiteAuthDoc    = []byte("// auth validate\n")
	suiteMainSource = []byte("package main\n")
)

// GraphFactory creates a Graph pre-loaded with the canonical test shape.
// The factory is responsible for cleanup (use t.Cleanup or t.TempDir).
type GraphFactory func(t *testing.T) Graph

// RunGraphSuite runs the full Graph interface contract suite against any
// implementation. Each test is independent — failures are isolated.
func RunGraphSuite(t *testing.T, factory GraphFactory) {
	t.Helper()

	// -- GetNode ----------------------------------------------------------

	t.Run("GetNode/dir", func(t *testing.T) {
		g := factory(t)
		n, err := g.GetNode("pkg/auth")
		require.NoError(t, err)
		assert.Equal(t, "pkg/auth", n.ID)
		assert.True(t, n.Mode.IsDir(), "pkg/auth should be a directory")
	})

	t.Run("GetNode/file", func(t *testing.T) {
		g := factory(t)
		n, err := g.GetNode("pkg/auth/source")
		require.NoError(t, err)
		assert.Equal(t, "pkg/auth/source", n.ID)
		assert.False(t, n.Mode.IsDir(), "source should be a file")
	})

	t.Run("GetNode/not_found", func(t *testing.T) {
		g := factory(t)
		_, err := g.GetNode("nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("GetNode/leading_slash", func(t *testing.T) {
		g := factory(t)
		n, err := g.GetNode("/pkg/auth")
		require.NoError(t, err)
		assert.Equal(t, "pkg/auth", n.ID)
	})

	// -- ListChildren -----------------------------------------------------

	t.Run("ListChildren/root", func(t *testing.T) {
		g := factory(t)
		children, err := g.ListChildren("")
		require.NoError(t, err)
		assert.Contains(t, children, "pkg")
	})

	t.Run("ListChildren/dir", func(t *testing.T) {
		g := factory(t)
		children, err := g.ListChildren("pkg")
		require.NoError(t, err)
		assert.Len(t, children, 3, "pkg should have auth, main, empty")
	})

	t.Run("ListChildren/leaf_dir", func(t *testing.T) {
		g := factory(t)
		children, err := g.ListChildren("pkg/auth")
		require.NoError(t, err)
		// Should have source + doc
		assert.Len(t, children, 2)
	})

	t.Run("ListChildren/empty_dir", func(t *testing.T) {
		g := factory(t)
		children, err := g.ListChildren("pkg/empty")
		require.NoError(t, err)
		assert.Empty(t, children)
	})

	// -- ListChildStats ---------------------------------------------------

	t.Run("ListChildStats/dir", func(t *testing.T) {
		g := factory(t)
		stats, err := g.ListChildStats("pkg")
		require.NoError(t, err)
		assert.Len(t, stats, 3)

		byID := map[string]NodeStat{}
		for _, s := range stats {
			byID[filepath.Base(s.ID)] = s
		}

		assert.True(t, byID["auth"].IsDir)
		assert.True(t, byID["main"].IsDir)
		assert.True(t, byID["empty"].IsDir)
	})

	t.Run("ListChildStats/leaf_dir_has_files", func(t *testing.T) {
		g := factory(t)
		stats, err := g.ListChildStats("pkg/auth")
		require.NoError(t, err)
		assert.Len(t, stats, 2)

		for _, s := range stats {
			assert.False(t, s.IsDir, "%s should be a file", s.ID)
			assert.Greater(t, s.ContentSize, int64(0), "%s should have content", s.ID)
		}
	})

	t.Run("ListChildStats/empty_dir", func(t *testing.T) {
		g := factory(t)
		stats, err := g.ListChildStats("pkg/empty")
		require.NoError(t, err)
		assert.Empty(t, stats)
	})

	t.Run("ListChildStats/not_found", func(t *testing.T) {
		g := factory(t)
		stats, err := g.ListChildStats("nonexistent")
		// Some impls return error, some return empty slice — both are valid
		if err == nil {
			assert.Empty(t, stats)
		}
	})

	// -- ReadContent ------------------------------------------------------

	t.Run("ReadContent/full", func(t *testing.T) {
		g := factory(t)
		buf := make([]byte, 100)
		n, err := g.ReadContent("pkg/auth/source", buf, 0)
		require.NoError(t, err)
		assert.Equal(t, suiteAuthSource, buf[:n])
	})

	t.Run("ReadContent/offset", func(t *testing.T) {
		g := factory(t)
		buf := make([]byte, 4)
		n, err := g.ReadContent("pkg/auth/source", buf, 5)
		require.NoError(t, err)
		assert.Equal(t, "Vali", string(buf[:n])) // "func [Vali]date..."
	})

	t.Run("ReadContent/not_found", func(t *testing.T) {
		g := factory(t)
		buf := make([]byte, 10)
		_, err := g.ReadContent("nonexistent", buf, 0)
		assert.Error(t, err)
	})

	// -- Invalidate -------------------------------------------------------

	t.Run("Invalidate/no_panic", func(t *testing.T) {
		g := factory(t)
		// Should not panic on valid or invalid IDs
		g.Invalidate("pkg/auth/source")
		g.Invalidate("nonexistent")
	})

	// -- Act --------------------------------------------------------------

	t.Run("Act/not_supported", func(t *testing.T) {
		g := factory(t)
		_, err := g.Act("pkg/auth", "click", "")
		assert.ErrorIs(t, err, ErrActNotSupported)
	})

	// -- GetCallers / GetCallees (baseline) -------------------------------

	t.Run("GetCallers/empty", func(t *testing.T) {
		g := factory(t)
		callers, err := g.GetCallers("UnknownToken")
		require.NoError(t, err)
		assert.Empty(t, callers)
	})

	t.Run("GetCallees/empty", func(t *testing.T) {
		g := factory(t)
		callees, err := g.GetCallees("pkg/auth")
		require.NoError(t, err)
		assert.Empty(t, callees)
	})
}

// ---------------------------------------------------------------------------
// Factories
// ---------------------------------------------------------------------------

func memoryStoreFactory(t *testing.T) Graph {
	t.Helper()
	s := NewMemoryStore()
	s.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		ModTime:  suiteModTime,
		Children: []string{"pkg/auth", "pkg/main", "pkg/empty"},
	})
	s.AddNode(&Node{
		ID:       "pkg/auth",
		Mode:     fs.ModeDir,
		ModTime:  suiteModTime,
		Children: []string{"pkg/auth/source", "pkg/auth/doc"},
	})
	s.AddNode(&Node{ID: "pkg/auth/source", Mode: 0o444, ModTime: suiteModTime, Data: suiteAuthSource})
	s.AddNode(&Node{ID: "pkg/auth/doc", Mode: 0o444, ModTime: suiteModTime, Data: suiteAuthDoc})
	s.AddNode(&Node{
		ID:       "pkg/main",
		Mode:     fs.ModeDir,
		ModTime:  suiteModTime,
		Children: []string{"pkg/main/source"},
	})
	s.AddNode(&Node{ID: "pkg/main/source", Mode: 0o444, ModTime: suiteModTime, Data: suiteMainSource})
	s.AddNode(&Node{ID: "pkg/empty", Mode: fs.ModeDir, ModTime: suiteModTime})
	return s
}

func hotSwapFactory(t *testing.T) Graph {
	t.Helper()
	return NewHotSwapGraph(memoryStoreFactory(t))
}

// sqliteGraphFactory creates a temp SQLite DB with a nodes table matching
// the canonical test graph, then opens it as a SQLiteGraph.
func sqliteGraphFactory(t *testing.T) Graph {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE results (id TEXT PRIMARY KEY, record JSON);
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON,
			source_file TEXT
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);
		CREATE TABLE node_refs (token TEXT, node_id TEXT);
	`)
	require.NoError(t, err)

	mtime := suiteModTime.UnixNano()
	rows := []struct {
		id, parentID, name string
		kind               int
		size               int
		record             string
	}{
		{"pkg", "", "pkg", 1, 0, ""},
		{"pkg/auth", "pkg", "auth", 1, 0, ""},
		{"pkg/auth/source", "pkg/auth", "source", 0, len(suiteAuthSource), string(suiteAuthSource)},
		{"pkg/auth/doc", "pkg/auth", "doc", 0, len(suiteAuthDoc), string(suiteAuthDoc)},
		{"pkg/main", "pkg", "main", 1, 0, ""},
		{"pkg/main/source", "pkg/main", "source", 0, len(suiteMainSource), string(suiteMainSource)},
		{"pkg/empty", "pkg", "empty", 1, 0, ""},
	}
	for _, r := range rows {
		_, err = db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, ?, ?, ?)",
			r.id, r.parentID, r.name, r.kind, r.size, mtime, r.record,
		)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	// Minimal schema — just needs Table set for the nodes-table path
	schema := &api.Topology{Table: "results"}
	g, err := OpenSQLiteGraph(dbPath, schema, stubRender)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// stubRender is a no-op renderer for SQLiteGraph tests where content is inline.
func stubRender(tmpl string, values map[string]any) (string, error) {
	return tmpl, nil
}

// ---------------------------------------------------------------------------
// Suite runners — one per implementation
// ---------------------------------------------------------------------------

func TestMemoryStore_GraphSuite(t *testing.T)  { RunGraphSuite(t, memoryStoreFactory) }
func TestHotSwapGraph_GraphSuite(t *testing.T) { RunGraphSuite(t, hotSwapFactory) }
func TestSQLiteGraph_GraphSuite(t *testing.T)  { RunGraphSuite(t, sqliteGraphFactory) }
