package graph

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestNodesTableReader_RestoresProperties covers the serve-time half of the
// Properties round-trip. SQLiteWriter marshals a dir node's Properties into the
// `record` column, and the build-time SQLiteWriter.GetNode restores them — but
// the serve-time reader used to ignore `record` entirely, so every construct
// read from a .db silently lost lang/pkg/imports. That loss is why qualified
// callee resolution had to fall back to regex-scraping context text
// (mache-f930b6).
func TestNodesTableReader_RestoresProperties(t *testing.T) {
	db := newPropsTestDB(t)

	imports, err := json.Marshal(map[string]string{"http": "net/http"})
	require.NoError(t, err)
	props, err := json.Marshal(map[string]json.RawMessage{
		"lang":    json.RawMessage(`"go"`),
		"imports": imports,
	})
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?,?,?,?,?,?,?)`,
		"pkg/Hello", "pkg", "Hello", NodeKindDir, 0, 0, string(props))
	require.NoError(t, err)

	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 8)
	node, err := r.GetNode("pkg/Hello")
	require.NoError(t, err)
	require.NotNil(t, node.Properties, "dir node Properties must survive the .db round-trip")
	require.Equal(t, "go", PropString(node, "lang"))

	// The structured import path loadImports() prefers must be reachable now.
	var got map[string]string
	require.NoError(t, json.Unmarshal(node.Properties["imports"], &got))
	require.Equal(t, map[string]string{"http": "net/http"}, got)
}

// TestNodesTableReader_FileRecordIsNotLoadedAsProperties pins the performance
// guard. For FILE nodes `record` holds rendered content, and this reader is
// deliberately lazy about content (ContentRef). The SQL-side CASE must keep
// SQLite from shipping a file's body just to answer GetNode — and that content
// must never be mistaken for Properties.
func TestNodesTableReader_FileRecordIsNotLoadedAsProperties(t *testing.T) {
	db := newPropsTestDB(t)

	body := `{"looks":"like json but is file content"}`
	_, err := db.Exec(
		`INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?,?,?,?,?,?,?)`,
		"pkg/file.go", "pkg", "file.go", NodeKindFile, len(body), 0, body)
	require.NoError(t, err)

	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 8)
	node, err := r.GetNode("pkg/file.go")
	require.NoError(t, err)
	require.Nil(t, node.Properties,
		"file-node record is content, not Properties — it must not be loaded here")
	require.NotNil(t, node.Ref, "file nodes stay lazy via ContentRef")
}

// TestNodesTableReader_NoPropertiesWhenRecordAbsent covers leyline-produced
// tables, which carry no mache Properties in `record` — GetNode must simply
// leave Properties nil rather than erroring.
func TestNodesTableReader_NoPropertiesWhenRecordAbsent(t *testing.T) {
	db := newPropsTestDB(t)
	_, err := db.Exec(
		`INSERT INTO nodes (id, parent_id, name, kind, size, mtime) VALUES (?,?,?,?,?,?)`,
		"pkg/Bare", "pkg", "Bare", NodeKindDir, 0, 0)
	require.NoError(t, err)

	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 8)
	node, err := r.GetNode("pkg/Bare")
	require.NoError(t, err)
	require.Nil(t, node.Properties)
}

// newPropsTestDB builds a nodes table WITHOUT the optional `context` column —
// matching what a real `mache build` (leyline backend) emits.
func newPropsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "props*.db")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db, err := sql.Open("sqlite", f.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
		kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL,
		record_id TEXT, record JSON, source_file TEXT
	)`)
	require.NoError(t, err)
	return db
}
