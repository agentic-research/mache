package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestCreateArena(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	arenaPath := filepath.Join(dir, "test.arena")

	// Create a small SQLite DB
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, val TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO t VALUES (1, 'hello')")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Create arena
	require.NoError(t, CreateArena(dbPath, arenaPath))

	// Verify header
	f, err := os.Open(arenaPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	h, err := ReadArenaHeader(f)
	require.NoError(t, err)
	assert.Equal(t, uint32(ArenaMagic), h.Magic)
	assert.Equal(t, uint8(ArenaVersion), h.Version)
	assert.Equal(t, uint8(0), h.ActiveBuffer)
	assert.Equal(t, uint64(1), h.Sequence)
	assert.NotZero(t, h.DataSize, "DataSize must be populated by CreateArena — readers hash buf[..DataSize] to verify against the controller's current_root")

	// Verify we can extract the DB back
	extractedPath, err := ExtractActiveDB(arenaPath)
	require.NoError(t, err)
	defer func() { _ = os.Remove(extractedPath) }()

	edb, err := sql.Open("sqlite", extractedPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = edb.Close() }()

	var val string
	require.NoError(t, edb.QueryRow("SELECT val FROM t WHERE id = 1").Scan(&val))
	assert.Equal(t, "hello", val)
}

func TestArenaFlusher_FlipBuffer(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "master.db")
	arenaPath := filepath.Join(dir, "test.arena")

	// Create initial DB
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, val TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO t VALUES (1, 'v1')")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Create arena from initial DB
	require.NoError(t, CreateArena(dbPath, arenaPath))

	// Modify the master DB
	db, err = sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE t SET val = 'v2' WHERE id = 1")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Flush to arena (no controller)
	flusher := NewArenaFlusher(arenaPath, dbPath, nil)
	require.NoError(t, flusher.FlushNow())

	// Verify header flipped
	f, err := os.Open(arenaPath)
	require.NoError(t, err)
	h, err := ReadArenaHeader(f)
	require.NoError(t, err)
	_ = f.Close()

	assert.Equal(t, uint8(1), h.ActiveBuffer, "should flip to buffer 1")
	assert.Equal(t, uint64(2), h.Sequence, "sequence should increment")

	// Extract and verify updated content
	extractedPath, err := ExtractActiveDB(arenaPath)
	require.NoError(t, err)
	defer func() { _ = os.Remove(extractedPath) }()

	edb, err := sql.Open("sqlite", extractedPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = edb.Close() }()

	var val string
	require.NoError(t, edb.QueryRow("SELECT val FROM t WHERE id = 1").Scan(&val))
	assert.Equal(t, "v2", val)
}

func TestArenaFlusher_Coalesce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "master.db")
	arenaPath := filepath.Join(dir, "test.arena")

	// Create DB
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, val TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO t VALUES (1, 'v1')")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, CreateArena(dbPath, arenaPath))

	flusher := NewArenaFlusher(arenaPath, dbPath, nil)
	flusher.Start(50 * time.Millisecond)

	// Fire 10 rapid RequestFlush calls (should coalesce into ~1-2 flushes)
	for range 10 {
		flusher.RequestFlush()
	}

	// WAIT FOR THE FLUSH, do not sleep a guess at how long it takes
	// (mache-3a2a8b). Sleeping 80ms of wall clock said nothing about whether
	// the coalescing goroutine had been SCHEDULED to run the flush; on a
	// loaded runner the tick fires and the goroutine has not had a slice yet,
	// leaving the sequence at 1. That is how this failed on CI for a PR that
	// touched only .gitignore.
	//
	// Waiting cannot inflate the number the ceiling below checks. `dirty` is
	// one bool: the 10 requests set it once, the first tick that sees it
	// clears it BEFORE flushing, and every later tick is a no-op. The
	// sequence reaches 2 and stays there however long this waits — so the
	// coalescing guarantee is enforced exactly as strictly as before.
	var h ArenaHeader
	require.Eventually(t, func() bool {
		f, err := os.Open(arenaPath)
		if err != nil {
			return false
		}
		defer func() { _ = f.Close() }()
		got, err := ReadArenaHeader(f)
		if err != nil {
			return false
		}
		h = *got
		return h.Sequence >= 2
	}, 5*time.Second, 5*time.Millisecond,
		"no flush ran after 10 RequestFlush calls on a 50ms ticker")

	// The coalescing guarantee: "10 rapid requests collapse to ≤2 flushes,"
	// not "exactly one flush." 3 allows a request landing across a tick
	// boundary. Either way it must be far below 11 (mache-02a9ab).
	assert.LessOrEqual(t, h.Sequence, uint64(3),
		"10 rapid requests must coalesce — observed sequence %d implies they did not", h.Sequence)

	require.NoError(t, flusher.Close())
}

// BenchmarkArenaFlush measures flush latency at various DB sizes.
// Run with: task test -- -run=^$ -bench=BenchmarkArenaFlush -benchmem ./graph/
func BenchmarkArenaFlush(b *testing.B) {
	for _, sizeKB := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("%dKB", sizeKB), func(b *testing.B) {
			dir := b.TempDir()
			dbPath := filepath.Join(dir, "master.db")
			arenaPath := filepath.Join(dir, "test.arena")

			// Create a DB of approximate target size by inserting rows
			db, err := sql.Open("sqlite", dbPath)
			require.NoError(b, err)
			_, err = db.Exec("PRAGMA journal_mode=DELETE")
			require.NoError(b, err)
			_, err = db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, val TEXT)")
			require.NoError(b, err)

			// ~100 bytes per row → sizeKB*1024/100 rows
			rowCount := sizeKB * 1024 / 100
			tx, err := db.Begin()
			require.NoError(b, err)
			stmt, err := tx.Prepare("INSERT INTO t VALUES (?, ?)")
			require.NoError(b, err)
			payload := "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" // ~90 chars
			for i := range rowCount {
				_, err = stmt.Exec(i, payload)
				require.NoError(b, err)
			}
			_ = stmt.Close()
			require.NoError(b, tx.Commit())
			require.NoError(b, db.Close())

			// Verify actual size
			fi, err := os.Stat(dbPath)
			require.NoError(b, err)
			b.Logf("DB size: %d KB (%d rows)", fi.Size()/1024, rowCount)

			// Create arena
			require.NoError(b, CreateArena(dbPath, arenaPath))

			flusher := NewArenaFlusher(arenaPath, dbPath, nil)

			b.ResetTimer()
			b.SetBytes(fi.Size())
			for i := 0; i < b.N; i++ {
				if err := flusher.FlushNow(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
