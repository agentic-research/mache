package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestNewASTWalker_LeavesConnUntuned proves NewASTWalker does NOT mutate the
// caller's connection: no single-conn pin, no process-lifetime EXCLUSIVE lock.
// This is the safety contract for the served/mounted path (mache-010123), where
// the walker runs against a SQLiteGraph's shared pool and pinning it to one
// connection or holding a file lock for the daemon's lifetime would serialize
// every NFS/MCP read and block external writers. The aggressive tuning lives in
// TuneReadConnForBuild, which only one-shot owners may call.
func TestNewASTWalker_LeavesConnUntuned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// A served SQLiteGraph sets a multi-connection pool; emulate that.
	db.SetMaxOpenConns(4)
	require.NoError(t, err)

	_ = NewASTWalker(db)
	require.Equal(t, 4, db.Stats().MaxOpenConnections,
		"NewASTWalker must not clobber the caller's pool size (mache-010123)")

	// A second independent handle to the same file must still be able to write
	// while the walker is live — i.e. no EXCLUSIVE lock is held.
	_, err = db.Exec("CREATE TABLE t(x)")
	require.NoError(t, err)
	other, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = other.Close() }()
	_, err = other.Exec("INSERT INTO t VALUES (1)")
	require.NoError(t, err, "NewASTWalker must not hold an EXCLUSIVE lock on a shared db")
}

// TestTuneReadConnForBuild_PinsConn is the counterpart: the build-only tuning
// DOES pin to a single connection (safe only because the caller exclusively
// owns the temp db). Guards against the tuning silently becoming a no-op.
func TestTuneReadConnForBuild_PinsConn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	TuneReadConnForBuild(db)
	require.Equal(t, 1, db.Stats().MaxOpenConnections,
		"build tuning pins the pool to a single connection")
}
