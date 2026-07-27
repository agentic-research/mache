package graph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNodesDB creates a nodes table with the given optional columns, mimicking
// the three producers mache must tolerate.
func newNodesDB(t *testing.T, withContext, withProps bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cols := "id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER, " +
		"size INTEGER, mtime INTEGER, record_id TEXT, record TEXT, source_file TEXT"
	if withContext {
		cols += ", context BLOB"
	}
	if withProps {
		cols += ", props JSON"
	}
	_, err = db.Exec("CREATE TABLE nodes (" + cols + ")")
	require.NoError(t, err)
	return db
}

func TestRequireProps_CurrentMacheDB_OK(t *testing.T) {
	assert.NoError(t, RequireProps(newNodesDB(t, true, true)))
}

// TestRequireProps_LeylineDB_OK is the regression guard for the scoping. A
// blanket "refuse anything without props" would reject every leyline-produced
// .db — and leyline parse has been mache's sole source parser since v0.18.0,
// so that would break the primary input rather than a legacy corner.
func TestRequireProps_LeylineDB_OK(t *testing.T) {
	assert.NoError(t, RequireProps(newNodesDB(t, false, false)),
		"a leyline-shaped nodes table must serve, not be refused")
}

func TestRequireProps_StaleMacheDB_Refused(t *testing.T) {
	err := RequireProps(newNodesDB(t, true, false))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStalePropsSchema)
	assert.Contains(t, err.Error(), "mache build",
		"the error must name the remedy, not just the symptom")
}
