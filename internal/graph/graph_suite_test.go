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

// SuiteOpts pins implementation-specific behaviour the Graph contract
// allows but where each backend has a definite answer. The suite's
// permissive defaults match older code; setting these lets the suite
// catch regressions in a specific backend instead of accepting the
// other valid behaviour silently.
//
// Bead mache-035f19 — without RootEmptyReturnsDir set, a regression
// where ntr.GetNode("") flipped to ErrNotFound would silently pass
// the WritableGraph suite while breaking FUSE/NFS root lookups in
// production.
type SuiteOpts struct {
	// RootEmptyReturnsDir asserts GetNode("") returns a synthetic dir
	// node. Backends backed by NodesTableReader (SQLiteGraph,
	// WritableGraph, HotSwapGraph) set this true; MemoryStore does
	// not — it returns ErrNotFound for the empty key.
	RootEmptyReturnsDir bool

	// GetNodePopulatesChildren pins whether this backend fills
	// GetNode().Children for directories, or leaves it empty and expects
	// callers to use ListChildren.
	//
	// The backends genuinely disagree today, and the split is not random:
	// MemoryStore (and HotSwapGraph, which wraps it) returns the live node so
	// Children is present, while EVERY SQLite-backed reader — SQLiteGraph,
	// NodesTableReader/WritableGraph, and SQLiteWriter — rebuilds a node from
	// a single row and leaves Children nil. Populating it would cost an extra
	// child query on what is the hottest read path in the system.
	//
	// This flag does NOT bless the divergence; it makes it explicit so it
	// cannot drift further while the contract is decided (mache-e3d9bb). Until
	// then, engine code must not branch on GetNode().Children — a guard that
	// did exactly that was live on memory and dead on the build path, dropping
	// 540 constructs from one Rust corpus (mache-c725e9).
	GetNodePopulatesChildren bool
}

// RunGraphSuiteWithOpts runs the full Graph interface contract suite against
// any implementation. Each test is independent — failures are isolated.
//
// Every backend passes its opts explicitly: a bare RunGraphSuite convenience
// wrapper existed and took SuiteOpts{}, which meant a new backend silently
// accepted the permissive defaults instead of stating its answer. Opts are
// where the contract's known variation lives, so making them mandatory is the
// point.
func RunGraphSuiteWithOpts(t *testing.T, factory GraphFactory, opts SuiteOpts) {
	t.Helper()

	// -- GetNode ----------------------------------------------------------

	t.Run("GetNode/root_empty_string", func(t *testing.T) {
		g := factory(t)
		n, err := g.GetNode("")
		if opts.RootEmptyReturnsDir {
			require.NoError(t, err, "this backend MUST return a synthetic root node for empty id")
			assert.Equal(t, "", n.ID)
			assert.True(t, n.Mode.IsDir(), "root must be a directory")
		} else {
			// Permissive default — kept so existing tests stay green
			// while we tighten implementations one at a time. Either
			// answer is allowed; FUSE/NFS layers handle root specially.
			if err == nil {
				assert.True(t, n.Mode.IsDir(), "root should be a directory")
			} else {
				assert.ErrorIs(t, err, ErrNotFound)
			}
		}
	})

	// The Graph contract exposes a node's children TWICE — as GetNode().Children
	// and via ListChildren() — and nothing made the two agree. SQLiteWriter's
	// GetNode never populated Children at all (it selects kind/mtime/record/
	// context/props and stops), which silently disabled a dedup guard in the
	// ingest engine that gated on `len(existing.Children) > 0`. That guard was
	// live on MemoryStore and dead on the build path: one engine, one input,
	// two backends, two different graphs, and 540 constructs dropped from a
	// single Rust corpus (mache-c725e9, mache-e3d9bb).
	//
	// A wide struct whose fields each backend populates at its own discretion
	// is not an interface — it is a suggestion. This asserts the two views
	// agree, so a backend cannot answer "no children" through one accessor and
	// "three children" through the other.
	t.Run("GetNode/children_agree_with_ListChildren", func(t *testing.T) {
		g := factory(t)
		for _, dir := range []string{"pkg", "pkg/auth", "pkg/main", "pkg/empty"} {
			listed, err := g.ListChildren(dir)
			require.NoError(t, err, "ListChildren(%s)", dir)

			n, err := g.GetNode(dir)
			require.NoError(t, err, "GetNode(%s)", dir)

			if opts.GetNodePopulatesChildren {
				assert.ElementsMatch(t, listed, n.Children,
					"this backend populates GetNode().Children, so it MUST agree with "+
						"ListChildren(%q)", dir)
				continue
			}
			assert.Empty(t, n.Children,
				"this backend is pinned as NOT populating GetNode().Children — if it "+
					"started, callers would silently change behaviour depending on "+
					"backend. Flip GetNodePopulatesChildren deliberately, don't drift "+
					"into it (mache-e3d9bb)")
		}
	})

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
		// Bead mache-0375fe — pin the nil-vs-empty contract.
		// Backends are free to return nil OR an empty slice; both
		// satisfy len() == 0. Pinning len explicitly catches silent
		// shape changes that would surprise JSON consumers.
		assert.Zero(t, len(children),
			"empty dir must return nil or zero-length slice (got non-empty %v)", children)
	})

	// -- ListChildStats ---------------------------------------------------

	t.Run("ListChildStats/root", func(t *testing.T) {
		g := factory(t)
		stats, err := g.ListChildStats("")
		if err == nil {
			// Should contain "pkg" as a directory
			require.NotEmpty(t, stats)
			assert.True(t, stats[0].IsDir)
		}
	})

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
		// Known inconsistency: MemoryStore returns ErrNotFound, SQLiteGraph
		// returns (nil, nil) because SELECT returns zero rows without error.
		// Both are acceptable — callers handle both empty and error.
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

// sqliteGraphFactory opens the canonical test DB as a SQLiteGraph (nodes-table fast path).
func sqliteGraphFactory(t *testing.T) Graph {
	t.Helper()
	dbPath := createNodesTableDB(t)
	schema := &api.Topology{Table: "results"}
	g, err := OpenSQLiteGraph(dbPath, schema, stubRender)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// stubRender is a no-op renderer for tests where content is inline in the record column.
func stubRender(tmpl string, values map[string]any) (string, error) {
	return tmpl, nil
}

// createNodesTableDB creates a temp SQLite DB with the canonical test graph
// in the nodes table schema. Shared by sqliteGraphFactory and writableGraphFactory.
func createNodesTableDB(t *testing.T) string {
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
			record TEXT,
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
	return dbPath
}

// writableGraphFactory opens the canonical test DB as a WritableGraph.
func writableGraphFactory(t *testing.T) Graph {
	t.Helper()
	dbPath := createNodesTableDB(t)
	schema := &api.Topology{Table: "results"}
	g, err := OpenWritableGraph(dbPath, schema, stubRender, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// ---------------------------------------------------------------------------
// Suite runners — one per implementation
// ---------------------------------------------------------------------------

// TestGraphBackends_Suite runs the contract suite against every Graph
// implementation. Adding a backend is one table row plus a factory — which is
// also why the entries live in a table rather than four near-identical
// functions: the previous shape duplicated the same call line per backend, so
// each new implementation added another copy.
//
// Each row states its answers explicitly. There is no permissive default to
// fall into: a backend whose behaviour is not yet pinned should be added with
// the answer it actually gives, and changed deliberately when the contract is.
func TestGraphBackends_Suite(t *testing.T) {
	backends := []struct {
		name    string
		factory GraphFactory
		opts    SuiteOpts
		why     string
	}{
		{
			name:    "MemoryStore",
			factory: memoryStoreFactory,
			opts:    SuiteOpts{GetNodePopulatesChildren: true},
			why:     "returns the live node pointer, so Children is present; ErrNotFound for GetNode(\"\")",
		},
		{
			name:    "HotSwapGraph",
			factory: hotSwapFactory,
			opts:    SuiteOpts{GetNodePopulatesChildren: true},
			why:     "wraps MemoryStore in this fixture and inherits both answers; swapping in an SQLiteGraph flips them, which is the point of HotSwap",
		},
		{
			name:    "SQLiteGraph",
			factory: sqliteGraphFactory,
			opts:    SuiteOpts{RootEmptyReturnsDir: true},
			why:     "rebuilds a node from one row, so Children is left to ListChildren",
		},
		{
			name:    "WritableGraph",
			factory: writableGraphFactory,
			opts:    SuiteOpts{RootEmptyReturnsDir: true},
			why:     "backed by NodesTableReader — same single-row rebuild, same answers",
		},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Logf("%s: %s", b.name, b.why)
			RunGraphSuiteWithOpts(t, b.factory, b.opts)
		})
	}
}
