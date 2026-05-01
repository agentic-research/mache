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

// TestStreamSQLite covers the streaming variant — lower memory
// footprint than LoadSQLite because only one parsed record is
// alive at a time. Production callers (engine.go) iterate large
// .db files this way.
func TestStreamSQLite(t *testing.T) {
	t.Run("yields parsed records in id order", func(t *testing.T) {
		dbPath := createTestDB(t, []string{
			`{"name":"alpha"}`,
			`{"name":"beta"}`,
			`{"name":"gamma"}`,
		})
		var ids []string
		var names []string
		err := StreamSQLite(dbPath, func(id string, rec any) error {
			ids = append(ids, id)
			m, ok := rec.(map[string]any)
			require.True(t, ok, "record should parse to map")
			names = append(names, m["name"].(string))
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, ids)
		assert.Equal(t, []string{"alpha", "beta", "gamma"}, names)
	})

	t.Run("propagates callback error and stops iteration", func(t *testing.T) {
		dbPath := createTestDB(t, []string{
			`{"name":"first"}`,
			`{"name":"second"}`,
			`{"name":"third"}`,
		})
		count := 0
		want := assert.AnError
		err := StreamSQLite(dbPath, func(_ string, _ any) error {
			count++
			if count == 2 {
				return want
			}
			return nil
		})
		assert.ErrorIs(t, err, want)
		assert.Equal(t, 2, count, "iteration must stop on first callback error")
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		err := StreamSQLite("/tmp/nonexistent_stream_test.db", func(string, any) error {
			t.Fatal("callback must not run for missing db")
			return nil
		})
		require.Error(t, err)
	})

	t.Run("invalid JSON in record column surfaces parse error", func(t *testing.T) {
		// JSON parsing happens up-front in StreamSQLite (vs StreamSQLiteRaw
		// which defers it to the worker). Bad JSON here = error here.
		dbPath := createTestDB(t, []string{`{not valid json`})
		err := StreamSQLite(dbPath, func(string, any) error {
			t.Fatal("callback must not run on parse failure")
			return nil
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse record json")
	})
}

// TestStreamSQLiteRaw covers the parse-deferred variant used by
// the parallel ingestion pipeline — workers handle JSON parsing
// on their own goroutines, so this loader hands them raw strings.
func TestStreamSQLiteRaw(t *testing.T) {
	t.Run("yields raw json strings unchanged", func(t *testing.T) {
		records := []string{
			`{"name":"alpha"}`,
			`{"name":"beta"}`,
			// Intentionally malformed — Raw doesn't parse, so this is fine.
			`{not valid json`,
		}
		dbPath := createTestDB(t, records)

		var ids, raws []string
		err := StreamSQLiteRaw(dbPath, func(id, raw string) error {
			ids = append(ids, id)
			raws = append(raws, raw)
			return nil
		})
		require.NoError(t, err, "Raw must not parse — malformed JSON is the worker's problem, not the loader's")
		assert.Equal(t, []string{"a", "b", "c"}, ids)
		assert.Equal(t, records, raws)
	})

	t.Run("propagates callback error", func(t *testing.T) {
		dbPath := createTestDB(t, []string{`{"a":1}`, `{"b":2}`})
		want := assert.AnError
		err := StreamSQLiteRaw(dbPath, func(string, string) error { return want })
		assert.ErrorIs(t, err, want)
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		err := StreamSQLiteRaw("/tmp/nonexistent_stream_raw_test.db", func(string, string) error {
			t.Fatal("callback must not run for missing db")
			return nil
		})
		require.Error(t, err)
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
