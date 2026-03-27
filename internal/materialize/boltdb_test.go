//go:build boltdb

package materialize

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

func TestBoltDBMaterializer_Interface(t *testing.T) {
	var _ Materializer = (*BoltDBMaterializer)(nil)
}

func TestBoltDBMaterializer_CreatesValidDB(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.bolt")

	m := &BoltDBMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	// Should be a valid BoltDB
	db, err := bolt.Open(outPath, 0o600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
}

func TestBoltDBMaterializer_CreatesBucketsForDirs(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.bolt")

	m := &BoltDBMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	db, err := bolt.Open(outPath, 0o600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	err = db.View(func(tx *bolt.Tx) error {
		// Top-level "functions" bucket should exist
		b := tx.Bucket([]byte("functions"))
		require.NotNil(t, b, "functions bucket should exist")

		// Nested "HandleRequest" bucket
		hr := b.Bucket([]byte("HandleRequest"))
		require.NotNil(t, hr, "functions/HandleRequest bucket should exist")

		// Nested "ProcessOrder" bucket
		po := b.Bucket([]byte("ProcessOrder"))
		require.NotNil(t, po, "functions/ProcessOrder bucket should exist")

		return nil
	})
	require.NoError(t, err)
}

func TestBoltDBMaterializer_WritesFileContent(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.bolt")

	m := &BoltDBMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	db, err := bolt.Open(outPath, 0o600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	err = db.View(func(tx *bolt.Tx) error {
		// functions/HandleRequest/source should be a key with content value
		b := tx.Bucket([]byte("functions"))
		require.NotNil(t, b)
		hr := b.Bucket([]byte("HandleRequest"))
		require.NotNil(t, hr)

		val := hr.Get([]byte("source"))
		assert.Equal(t, "func HandleRequest() {}", string(val))

		return nil
	})
	require.NoError(t, err)
}

func TestBoltDBMaterializer_RootLevelFiles(t *testing.T) {
	srcDB := setupTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.bolt")

	m := &BoltDBMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	db, err := bolt.Open(outPath, 0o600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	err = db.View(func(tx *bolt.Tx) error {
		// _schema.json is a root-level file — stored in a "_root" bucket
		root := tx.Bucket([]byte("_root"))
		require.NotNil(t, root, "_root bucket should exist for root-level files")

		val := root.Get([]byte("_schema.json"))
		assert.Equal(t, `{"version":"v1"}`, string(val))

		return nil
	})
	require.NoError(t, err)
}

func TestBoltDBMaterializer_EmptyRootNode(t *testing.T) {
	// Schemas like trivy-v2 use "name": "" for the root node, creating
	// an empty-string directory node. Children should become top-level buckets.
	srcDB := setupEmptyRootTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out.bolt")

	m := &BoltDBMaterializer{}
	err := m.Materialize(srcDB, outPath)
	require.NoError(t, err)

	db, err := bolt.Open(outPath, 0o600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	err = db.View(func(tx *bolt.Tx) error {
		// "alpine 3.18" should be a top-level bucket (not nested under empty root)
		platform := tx.Bucket([]byte("alpine 3.18"))
		require.NotNil(t, platform, "alpine 3.18 bucket should exist at top level")

		pkg := platform.Bucket([]byte("curl"))
		require.NotNil(t, pkg, "curl bucket should exist under alpine 3.18")

		advisory := pkg.Get([]byte("CVE-2024-1234"))
		assert.Equal(t, `{"FixedVersion":"8.5.0-r0","Status":0}`, string(advisory))

		return nil
	})
	require.NoError(t, err)
}

func TestBoltDBMaterializer_InvalidSource(t *testing.T) {
	m := &BoltDBMaterializer{}
	err := m.Materialize("/nonexistent/path.db", filepath.Join(t.TempDir(), "out.bolt"))
	assert.Error(t, err)
}

// setupEmptyRootTestDB creates a nodes table with an empty-named root node
// (the trivy-v2 schema pattern) and children that should become top-level buckets.
func setupEmptyRootTestDB(t *testing.T) string {
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

		-- Empty root node (trivy schema uses "name": "")
		INSERT INTO nodes VALUES ('', '', '', 1, 0, 1000, NULL);

		-- Platform bucket (child of empty root)
		INSERT INTO nodes VALUES ('alpine 3.18', '', 'alpine 3.18', 1, 0, 2000, NULL);

		-- Package bucket
		INSERT INTO nodes VALUES ('alpine 3.18/curl', 'alpine 3.18', 'curl', 1, 0, 2000, NULL);

		-- Advisory file (CVE ID as filename)
		INSERT INTO nodes VALUES ('alpine 3.18/curl/CVE-2024-1234', 'alpine 3.18/curl', 'CVE-2024-1234', 0, 42, 3000, '{"FixedVersion":"8.5.0-r0","Status":0}');

		-- _schema.json at root
		INSERT INTO nodes VALUES ('_schema.json', '', '_schema.json', 0, 14, 4000, '{"version":"v1"}');
	`)
	require.NoError(t, err)

	dbPath := filepath.Join(t.TempDir(), "source.db")
	_, err = db.Exec(`VACUUM INTO ?`, dbPath)
	require.NoError(t, err)

	return dbPath
}

func TestForFormat_BoltDB(t *testing.T) {
	m, err := ForFormat("boltdb")
	require.NoError(t, err)
	assert.IsType(t, &BoltDBMaterializer{}, m)
}
