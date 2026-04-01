//go:build boltdb

package materialize

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
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

func TestForFormat_BoltDB(t *testing.T) {
	m, err := ForFormat("boltdb")
	require.NoError(t, err)
	assert.IsType(t, &BoltDBMaterializer{}, m)
}
