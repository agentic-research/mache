package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #7: No method callees in --out mode / inconsistent with mount
// callees/ should be materialized in the output DB just like callers/.

func TestMaterializeCallees(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Create the schema that materializeVirtuals expects
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT,
			kind INTEGER,
			size INTEGER,
			mtime INTEGER,
			record TEXT
		);
		CREATE TABLE node_refs (
			token TEXT,
			node_id TEXT,
			PRIMARY KEY (token, node_id)
		);
		CREATE TABLE node_defs (
			token TEXT,
			dir_id TEXT,
			PRIMARY KEY (token, dir_id)
		);
	`)
	require.NoError(t, err)

	now := time.Now().UnixNano()

	// Insert some nodes: package/functions/FuncA with source that calls FuncB
	inserts := []struct {
		id, parentID, name string
		kind               int
		content            string
	}{
		{"pkg", "", "pkg", 1, ""},
		{"pkg/functions", "pkg", "functions", 1, ""},
		{"pkg/functions/FuncA", "pkg/functions", "FuncA", 1, ""},
		{"pkg/functions/FuncA/source", "pkg/functions/FuncA", "source", 0, "func FuncA() { FuncB() }"},
		{"pkg/functions/FuncB", "pkg/functions", "FuncB", 1, ""},
		{"pkg/functions/FuncB/source", "pkg/functions/FuncB", "source", 0, "func FuncB() { return }"},
	}

	for _, ins := range inserts {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, ?, ?, ?)",
			ins.id, ins.parentID, ins.name, ins.kind, len(ins.content), now, ins.content,
		)
		require.NoError(t, err)
	}

	// Insert refs: FuncA calls FuncB
	_, err = db.Exec("INSERT INTO node_refs (token, node_id) VALUES (?, ?)", "FuncB", "pkg/functions/FuncA")
	require.NoError(t, err)

	// Insert defs: FuncA and FuncB are defined
	_, err = db.Exec("INSERT INTO node_defs (token, dir_id) VALUES (?, ?)", "FuncA", "pkg/functions/FuncA")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO node_defs (token, dir_id) VALUES (?, ?)", "FuncB", "pkg/functions/FuncB")
	require.NoError(t, err)

	// Materialize callers (existing)
	tx, err := db.Begin()
	require.NoError(t, err)
	err = materializeCallers(tx, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// Verify callers/ exists for FuncB (FuncA calls FuncB)
	var callersDirID string
	err = db.QueryRow("SELECT id FROM nodes WHERE id = ?", "pkg/functions/FuncB/callers").Scan(&callersDirID)
	require.NoError(t, err, "callers/ directory should be materialized for FuncB")

	// Now test: materialize callees (the new feature)
	tx2, err := db.Begin()
	require.NoError(t, err)
	err = materializeCallees(tx2, now)
	require.NoError(t, err)
	require.NoError(t, tx2.Commit())

	// Verify callees/ exists for FuncA (FuncA calls FuncB)
	var calleesDirID string
	err = db.QueryRow("SELECT id FROM nodes WHERE id = ?", "pkg/functions/FuncA/callees").Scan(&calleesDirID)
	require.NoError(t, err, "callees/ directory should be materialized for FuncA")

	// Verify callee entry points to FuncB
	var calleeEntryID string
	err = db.QueryRow("SELECT id FROM nodes WHERE parent_id = ? AND name LIKE '%FuncB%'",
		"pkg/functions/FuncA/callees").Scan(&calleeEntryID)
	assert.NoError(t, err, "callees/ should contain an entry for FuncB")
}

// Issue #12: _project_files stores full content in --out mode
func TestMaterializeVirtuals_ProjectFilesMetadataOnly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT,
			kind INTEGER,
			size INTEGER,
			mtime INTEGER,
			record TEXT
		);
	`)
	require.NoError(t, err)

	now := time.Now().UnixNano()
	bigContent := "This is a large config file with lots of content..."

	// Insert _project_files directory and some file nodes with full content
	inserts := []struct {
		id, parentID, name string
		kind               int
		content            string
	}{
		{"_project_files", "", "_project_files", 1, ""},
		{"_project_files/README.md", "_project_files", "README.md", 0, bigContent},
		{"_project_files/docs", "_project_files", "docs", 1, ""},
		{"_project_files/docs/config.yaml", "_project_files/docs", "config.yaml", 0, bigContent},
		// Non-project-files node should NOT be stripped
		{"functions", "", "functions", 1, ""},
		{"functions/Foo", "functions", "Foo", 1, ""},
		{"functions/Foo/source", "functions/Foo", "source", 0, "func Foo() {}"},
	}

	for _, ins := range inserts {
		_, err := db.Exec(
			"INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record) VALUES (?, ?, ?, ?, ?, ?, ?)",
			ins.id, ins.parentID, ins.name, ins.kind, len(ins.content), now, ins.content,
		)
		require.NoError(t, err)
	}

	// Run stripProjectFileContent
	tx, err := db.Begin()
	require.NoError(t, err)
	err = stripProjectFileContent(tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// _project_files/ file nodes should have NULL record and size 0
	var record sql.NullString
	var size int
	err = db.QueryRow("SELECT record, size FROM nodes WHERE id = ?", "_project_files/README.md").Scan(&record, &size)
	require.NoError(t, err)
	assert.False(t, record.Valid, "_project_files file record should be NULL")
	assert.Equal(t, 0, size, "_project_files file size should be 0")

	err = db.QueryRow("SELECT record, size FROM nodes WHERE id = ?", "_project_files/docs/config.yaml").Scan(&record, &size)
	require.NoError(t, err)
	assert.False(t, record.Valid, "nested _project_files file record should be NULL")
	assert.Equal(t, 0, size, "nested _project_files file size should be 0")

	// _project_files directory nodes should be untouched
	var dirRecord sql.NullString
	err = db.QueryRow("SELECT record FROM nodes WHERE id = ?", "_project_files").Scan(&dirRecord)
	require.NoError(t, err)

	// Non-_project_files nodes should be untouched
	var sourceRecord string
	err = db.QueryRow("SELECT record FROM nodes WHERE id = ?", "functions/Foo/source").Scan(&sourceRecord)
	require.NoError(t, err)
	assert.Equal(t, "func Foo() {}", sourceRecord, "non-project-files content should be preserved")
}
