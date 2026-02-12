package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func createTestDB(t *testing.T, records []string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TABLE results (id TEXT PRIMARY KEY, record TEXT NOT NULL)")
	require.NoError(t, err)

	for i, rec := range records {
		_, err = db.Exec("INSERT INTO results (id, record) VALUES (?, ?)",
			string(rune('a'+i)), rec)
		require.NoError(t, err)
	}
	return dbPath
}

func TestLoadSQLite(t *testing.T) {
	t.Run("basic records", func(t *testing.T) {
		dbPath := createTestDB(t, []string{
			`{"item":{"name":"Alice","role":"admin"}}`,
			`{"item":{"name":"Bob","role":"user"}}`,
		})

		records, err := LoadSQLite(dbPath)
		require.NoError(t, err)
		assert.Len(t, records, 2)

		// Records are parsed JSON — map[string]any
		first, ok := records[0].(map[string]any)
		require.True(t, ok)
		item, ok := first["item"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "Alice", item["name"])
	})

	t.Run("empty database", func(t *testing.T) {
		dbPath := createTestDB(t, nil)

		records, err := LoadSQLite(dbPath)
		require.NoError(t, err)
		assert.Len(t, records, 0)
	})

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := LoadSQLite("/tmp/nonexistent_test.db")
		require.Error(t, err)
	})

	t.Run("nested structures preserved", func(t *testing.T) {
		dbPath := createTestDB(t, []string{
			`{"item":{"cve":{"id":"CVE-2024-0001","descriptions":[{"lang":"en","value":"test desc"}]}}}`,
		})

		records, err := LoadSQLite(dbPath)
		require.NoError(t, err)
		assert.Len(t, records, 1)

		rec := records[0].(map[string]any)
		item := rec["item"].(map[string]any)
		cve := item["cve"].(map[string]any)
		assert.Equal(t, "CVE-2024-0001", cve["id"])

		descs := cve["descriptions"].([]any)
		assert.Len(t, descs, 1)
		desc := descs[0].(map[string]any)
		assert.Equal(t, "test desc", desc["value"])
	})
}

func TestLoadSQLite_Integration(t *testing.T) {
	kevDB := os.Getenv("MACHE_TEST_KEV_DB")
	if kevDB == "" {
		t.Skip("MACHE_TEST_KEV_DB not set")
	}
	if _, err := os.Stat(kevDB); os.IsNotExist(err) {
		t.Skip("KEV database not found at " + kevDB)
	}

	records, err := LoadSQLite(kevDB)
	require.NoError(t, err)
	assert.Greater(t, len(records), 1000, "KEV should have >1000 records")

	// Spot-check structure
	first, ok := records[0].(map[string]any)
	require.True(t, ok, "record should be a map")
	_, hasItem := first["item"]
	assert.True(t, hasItem, "record should have 'item' key")
}
