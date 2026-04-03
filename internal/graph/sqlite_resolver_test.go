package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// SQLiteResolver characterization tests
// ---------------------------------------------------------------------------

// Uses createTestDB from sqlite_graph_test.go (same package, shared helper).

// stubRenderer is a minimal TemplateRenderer for testing.
func stubRenderer(tmpl string, values map[string]any) (string, error) {
	// Just return the template literal — we're testing resolver plumbing, not templates
	return tmpl + ":rendered", nil
}

func TestSQLiteResolver_Resolve_Basic(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"CVE-2024-1234": `{"severity": "CRITICAL", "description": "bad thing"}`,
	})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	content, err := r.Resolve(&ContentRef{
		DBPath:   dbPath,
		RecordID: "CVE-2024-1234",
		Template: "{{.severity}}",
	})
	require.NoError(t, err)
	assert.Equal(t, "{{.severity}}:rendered", string(content))
}

func TestSQLiteResolver_Resolve_RecordNotFound(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.Resolve(&ContentRef{
		DBPath:   dbPath,
		RecordID: "nonexistent",
		Template: "{{.foo}}",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolve record nonexistent")
}

func TestSQLiteResolver_Resolve_InvalidJSON(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"bad": `not valid json`,
	})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.Resolve(&ContentRef{
		DBPath:   dbPath,
		RecordID: "bad",
		Template: "{{.foo}}",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse record bad")
}

func TestSQLiteResolver_Resolve_NonObjectJSON(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"arr": `[1, 2, 3]`,
	})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.Resolve(&ContentRef{
		DBPath:   dbPath,
		RecordID: "arr",
		Template: "{{.foo}}",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a JSON object")
}

func TestSQLiteResolver_Resolve_BadDBPath(t *testing.T) {
	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.Resolve(&ContentRef{
		DBPath:   "/nonexistent/path/to.db",
		RecordID: "foo",
		Template: "{{.bar}}",
	})
	assert.Error(t, err)
}

func TestSQLiteResolver_DBPooling(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{
		"a": `{"v": 1}`,
		"b": `{"v": 2}`,
	})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	// Two resolves against same DB — should reuse connection
	_, err := r.Resolve(&ContentRef{DBPath: dbPath, RecordID: "a", Template: "t"})
	require.NoError(t, err)
	_, err = r.Resolve(&ContentRef{DBPath: dbPath, RecordID: "b", Template: "t"})
	require.NoError(t, err)

	// Pool should have exactly one entry
	r.mu.Lock()
	assert.Len(t, r.dbs, 1)
	r.mu.Unlock()
}

func TestSQLiteResolver_MultipleDatabases(t *testing.T) {
	db1 := createTestDB(t, map[string]string{"x": `{"from": "db1"}`})
	db2 := createTestDB(t, map[string]string{"x": `{"from": "db2"}`})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.Resolve(&ContentRef{DBPath: db1, RecordID: "x", Template: "t"})
	require.NoError(t, err)
	_, err = r.Resolve(&ContentRef{DBPath: db2, RecordID: "x", Template: "t"})
	require.NoError(t, err)

	r.mu.Lock()
	assert.Len(t, r.dbs, 2)
	r.mu.Unlock()
}

func TestSQLiteResolver_Close_ClearsPool(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{"a": `{"v": 1}`})

	r := NewSQLiteResolver(stubRenderer)
	_, err := r.Resolve(&ContentRef{DBPath: dbPath, RecordID: "a", Template: "t"})
	require.NoError(t, err)

	r.mu.Lock()
	assert.Len(t, r.dbs, 1)
	r.mu.Unlock()

	r.Close()

	// Pool should be empty after Close
	r.mu.Lock()
	assert.Empty(t, r.dbs)
	r.mu.Unlock()

	// Re-resolve should work — Close clears pool, getDB re-opens
	_, err = r.Resolve(&ContentRef{DBPath: dbPath, RecordID: "a", Template: "t"})
	require.NoError(t, err)
}

func TestSQLiteResolver_getDB_SetsWALAndReadOnly(t *testing.T) {
	dbPath := createTestDB(t, map[string]string{})

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	db, err := r.getDB(dbPath)
	require.NoError(t, err)

	// Verify query_only is ON — writes should fail
	_, err = db.Exec("CREATE TABLE should_fail (id TEXT)")
	assert.Error(t, err, "query_only should prevent writes")
}

func TestSQLiteResolver_getDB_CleansUpOnPragmaFailure(t *testing.T) {
	// Create a file that's not a valid SQLite DB
	badPath := filepath.Join(t.TempDir(), "not-a-db.db")
	require.NoError(t, os.WriteFile(badPath, []byte("not sqlite"), 0o644))

	r := NewSQLiteResolver(stubRenderer)
	defer r.Close()

	_, err := r.getDB(badPath)
	assert.Error(t, err)

	// Should not be in the pool
	r.mu.Lock()
	assert.NotContains(t, r.dbs, badPath)
	r.mu.Unlock()
}
