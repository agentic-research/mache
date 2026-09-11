package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/sqlcount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cold-path gate for projecting ONE source file (mache-40ce82).
//
// TestExtractCalls_StatementCountDoesNotGrowWithFileSize pins a single walker
// call. This pins what the build actually does per file — Engine.ingestSourceFile
// end to end — on a db the pinned leyline parsed, so every table shape is the
// real one (_ast, _source in path mode, _imports).
//
// The number is a COUNT of SQL statements, for the reasons the ExtractCalls gate
// spells out; and it is an EXACT count, not a bound. A file's whole section of
// the db — its nodes⋈_ast rows, its _source row, its _imports rows — is three
// statements' worth of data, and every question the projection asks about the
// file (language, package, source bytes, doc comments, calls, context, imports)
// is answered from that section in memory. Anything above three is a question
// being asked of SQLite per pattern or per construct again, which is exactly
// the shape of the O(nodes²) regression that shipped once (mache-4f3840).
//
// The walker is warmed on ONE other file first, so one-time work (the
// `_imports` table probe) is out of the measurement, and the file under test is
// one the walker has never seen — the count is the cold cost, not a cache hit.

const (
	// goStatementsPerFile is the whole section: nodes⋈_ast, node_child (the
	// file's child lists, from which each node's field is derived), _source,
	// _imports.
	goStatementsPerFile = 4
	// nonGoStatementsPerFile drops _imports, which is Go-only.
	nonGoStatementsPerFile = 3
)

// goCorpusFile writes a Go file with n functions, each documented, each
// calling a distinct helper through a package qualifier and bare — the shapes
// the doc-comment, call and import extracts all fire on.
func goCorpusFile(n int) string {
	var sb strings.Builder
	sb.WriteString("package corpus\n\nimport \"fmt\"\n\n")
	for i := range n {
		fmt.Fprintf(&sb, "// F%d is construct %d.\nfunc F%d() {\n\tfmt.Println(%d)\n\thelper%d()\n}\n\n", i, i, i, i, i)
	}
	return sb.String()
}

// pyCorpusFile writes a Python file with n functions, each calling a distinct
// helper — the python schema projects them as functions/.
func pyCorpusFile(n int) string {
	var sb strings.Builder
	sb.WriteString("import os\n\n")
	for i := range n {
		fmt.Fprintf(&sb, "def f%d():\n    helper%d()\n    return os.getcwd()\n\n", i, i)
	}
	return sb.String()
}

// projectionProbe holds one leyline-parsed corpus opened through the counting
// driver, with an Engine projecting it into a MemoryStore.
type projectionProbe struct {
	dir    string
	engine *Engine
	store  *graph.MemoryStore
}

// newProjectionProbe writes corpus into a temp dir, parses it with the pinned
// leyline, and wires an Engine for schemaPath whose walker reads the parse db
// through internal/sqlcount's counting driver.
func newProjectionProbe(t *testing.T, schemaPath string, corpus map[string]string) *projectionProbe {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for name, content := range corpus {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	dbPath := parseWithLeyline(t, dir)

	sqlcount.RegisterDriver()
	db, err := sql.Open(sqlcount.DriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	store := graph.NewMemoryStore()
	engine := NewEngine(loadSchema(t, schemaPath), store)
	engine.RootPath = dir
	engine.SetASTWalker(NewASTWalker(db))
	return &projectionProbe{dir: dir, engine: engine, store: store}
}

// project ingests one corpus file and returns the number of graph nodes it
// produced. The file's statements are counted by the caller.
func (p *projectionProbe) project(t *testing.T, name, lang string) int {
	t.Helper()
	path := filepath.Join(p.dir, name)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, p.engine.ingestSourceFile(path, lang, info.ModTime()))
	return len(p.store.NodesForPath(path))
}

// coldFileStatements warms the walker on warm, then counts the statements
// projecting target — a file the walker has never seen — issues. Returns the
// count and the number of nodes target projected to.
func coldFileStatements(t *testing.T, p *projectionProbe, warm, target, lang string) (int64, int) {
	t.Helper()
	p.project(t, warm, lang)
	count := sqlcount.Reset()
	nodes := p.project(t, target, lang)
	return count(), nodes
}

// TestProjectSourceFile_StatementCountIsTheSection is the gate.
func TestProjectSourceFile_StatementCountIsTheSection(t *testing.T) {
	const small, large = 10, 500

	goProbe := newProjectionProbe(t, "../../examples/go-schema.json", map[string]string{
		"warm.go":  goCorpusFile(3),
		"small.go": goCorpusFile(small),
		"large.go": goCorpusFile(large),
	})
	atSmall, smallNodes := coldFileStatements(t, goProbe, "warm.go", "small.go", "go")
	atLarge, largeNodes := coldFileStatements(t, goProbe, "warm.go", "large.go", "go")

	// Non-vacuity: the work still happens, and scales with the file, even
	// though the statement count does not.
	require.GreaterOrEqual(t, smallNodes, small, "the %d-function file projected only %d nodes", small, smallNodes)
	require.GreaterOrEqual(t, largeNodes, large, "the %d-function file projected only %d nodes", large, largeNodes)

	assert.Equal(t, int64(goStatementsPerFile), atSmall,
		"projecting a %d-construct Go file issued %d statements; its section of the db is %d",
		small, atSmall, goStatementsPerFile)
	assert.Equal(t, atSmall, atLarge, fmt.Sprintf(
		"projecting a Go file issued %d statements at %d constructs but %d at %d.\n\n"+
			"The count must not depend on how many constructs a file has. Growth here is a\n"+
			"per-construct or per-pattern query — the shape of the O(nodes²) projection\n"+
			"regression (mache-4f3840) that shipped through green CI once already.",
		atSmall, small, atLarge, large))

	pyProbe := newProjectionProbe(t, "../../examples/python-schema.json", map[string]string{
		"warm.py":   pyCorpusFile(3),
		"target.py": pyCorpusFile(small),
	})
	atPy, pyNodes := coldFileStatements(t, pyProbe, "warm.py", "target.py", "python")
	require.GreaterOrEqual(t, pyNodes, small, "the %d-function Python file projected only %d nodes", small, pyNodes)
	assert.Equal(t, int64(nonGoStatementsPerFile), atPy,
		"projecting a Python file issued %d statements; its section of the db is %d (no _imports)",
		atPy, nonGoStatementsPerFile)
}
