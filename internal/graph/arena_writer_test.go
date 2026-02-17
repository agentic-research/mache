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
	assert.Equal(t, uint8(1), h.Version)
	assert.Equal(t, uint8(0), h.ActiveBuffer)
	assert.Equal(t, uint64(1), h.Sequence)

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
	for i := 0; i < 10; i++ {
		flusher.RequestFlush()
	}

	// Wait for at least one tick to fire
	time.Sleep(80 * time.Millisecond)

	// Verify the arena was actually flushed (header should have changed)
	f, err := os.Open(arenaPath)
	require.NoError(t, err)
	h, err := ReadArenaHeader(f)
	require.NoError(t, err)
	_ = f.Close()

	// Sequence should be 2 (initial=1, one coalesced flush), not 11
	assert.Equal(t, uint64(2), h.Sequence, "10 rapid requests should coalesce into 1 flush")

	require.NoError(t, flusher.Close())
}

// BenchmarkArenaFlush measures flush latency at various DB sizes.
// Run with: task test -- -run=^$ -bench=BenchmarkArenaFlush -benchmem ./internal/graph/
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
			for i := 0; i < rowCount; i++ {
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
