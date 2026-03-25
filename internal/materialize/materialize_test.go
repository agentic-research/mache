package materialize

import (
	"archive/zip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// setupTestDB creates an in-memory SQLite DB with the standard mache nodes
// schema and some sample data, then vacuums it to a file so materializers
// can open it by path.
func setupTestDB(t *testing.T) string {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record TEXT
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);

		-- Root dirs
		INSERT INTO nodes VALUES ('functions', '', 'functions', 1, 0, 1000, NULL);

		-- Function: HandleRequest
		INSERT INTO nodes VALUES ('functions/HandleRequest', 'functions', 'HandleRequest', 1, 0, 2000, NULL);
		INSERT INTO nodes VALUES ('functions/HandleRequest/source', 'functions/HandleRequest', 'source', 0, 25, 3000, 'func HandleRequest() {}');

		-- Function: ProcessOrder
		INSERT INTO nodes VALUES ('functions/ProcessOrder', 'functions', 'ProcessOrder', 1, 0, 2000, NULL);
		INSERT INTO nodes VALUES ('functions/ProcessOrder/source', 'functions/ProcessOrder', 'source', 0, 22, 3000, 'func ProcessOrder() {}');

		-- _schema.json at root
		INSERT INTO nodes VALUES ('_schema.json', '', '_schema.json', 0, 14, 4000, '{"version":"v1"}');
	`)
	require.NoError(t, err)

	dbPath := filepath.Join(t.TempDir(), "source.db")
	_, err = db.Exec(`VACUUM INTO ?`, dbPath)
	require.NoError(t, err)

	return dbPath
}

// ---------------------------------------------------------------------------
// Materializer interface tests
// ---------------------------------------------------------------------------

func TestMaterializerInterface(t *testing.T) {
	// All materializers must satisfy the Materializer interface.
	var _ Materializer = (*SQLiteMaterializer)(nil)
	var _ Materializer = (*ZIPMaterializer)(nil)
}

// ---------------------------------------------------------------------------
// SQLite materializer
// ---------------------------------------------------------------------------

func TestSQLiteMaterializer_CopiesAllNodes(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.db")

	m := &SQLiteMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	// Open output and verify all nodes present
	db, err := sql.Open("sqlite", outPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 6, count) // 3 dirs (functions, HandleRequest, ProcessOrder) + 2 source files + _schema.json

	// Verify specific content preserved
	var record string
	err = db.QueryRow(`SELECT record FROM nodes WHERE id = 'functions/HandleRequest/source'`).Scan(&record)
	require.NoError(t, err)
	assert.Equal(t, "func HandleRequest() {}", record)
}

func TestSQLiteMaterializer_OutputFileExists(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.db")

	m := &SQLiteMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	info, err := os.Stat(outPath)
	require.NoError(t, err)
	assert.True(t, info.Size() > 0)
}

func TestSQLiteMaterializer_InvalidSource(t *testing.T) {
	m := &SQLiteMaterializer{}
	err := m.Materialize("/nonexistent/path.db", filepath.Join(t.TempDir(), "out.db"))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// ZIP materializer
// ---------------------------------------------------------------------------

func TestZIPMaterializer_CreatesValidArchive(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.zip")

	m := &ZIPMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	// Open and verify it's a valid zip
	r, err := zip.OpenReader(outPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	assert.True(t, len(r.File) > 0)
}

func TestZIPMaterializer_ContainsAllFiles(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.zip")

	m := &ZIPMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	r, err := zip.OpenReader(outPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	// Collect file names from the archive
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)

	// Should contain the 3 file nodes (kind=0), not directory nodes
	assert.Contains(t, names, "functions/HandleRequest/source")
	assert.Contains(t, names, "functions/ProcessOrder/source")
	assert.Contains(t, names, "_schema.json")
}

func TestZIPMaterializer_PreservesContent(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.zip")

	m := &ZIPMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	r, err := zip.OpenReader(outPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	// Find and read HandleRequest/source
	for _, f := range r.File {
		if f.Name == "functions/HandleRequest/source" {
			rc, err := f.Open()
			require.NoError(t, err)
			buf, readErr := io.ReadAll(rc)
			_ = rc.Close()
			require.NoError(t, readErr)
			assert.Equal(t, "func HandleRequest() {}", string(buf))
			return
		}
	}
	t.Fatal("functions/HandleRequest/source not found in zip")
}

func TestZIPMaterializer_SkipsNullContent(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.zip")

	m := &ZIPMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	r, err := zip.OpenReader(outPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	// Directory nodes (kind=1) should not appear as files in the zip
	for _, f := range r.File {
		assert.NotEqual(t, "functions", f.Name, "bare directory should not be a zip entry")
		assert.NotEqual(t, "functions/HandleRequest", f.Name, "bare directory should not be a zip entry")
	}
}

func TestZIPMaterializer_InvalidSource(t *testing.T) {
	m := &ZIPMaterializer{}
	err := m.Materialize("/nonexistent/path.db", filepath.Join(t.TempDir(), "out.zip"))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// ForFormat factory
// ---------------------------------------------------------------------------

func TestForFormat_SQLite(t *testing.T) {
	m, err := ForFormat("sqlite")
	require.NoError(t, err)
	assert.IsType(t, &SQLiteMaterializer{}, m)
}

func TestForFormat_Zip(t *testing.T) {
	m, err := ForFormat("zip")
	require.NoError(t, err)
	assert.IsType(t, &ZIPMaterializer{}, m)
}

func TestForFormat_Unknown(t *testing.T) {
	_, err := ForFormat("parquet")
	assert.Error(t, err)
}
