package leylinegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/projcfg"
)

// ParseCacheDirEnv points the parse cache somewhere other than ~/.mache/parse.
// `task bench:cold` sets it to a throwaway directory so the cold arm measures
// a genuinely cold parse and the warm arm measures a genuinely warm one,
// without touching the developer's real cache.
const ParseCacheDirEnv = "MACHE_PARSE_CACHE_DIR"

// parseCacheEntry is a reserved persistent parse db, held under an exclusive
// lock for as long as the caller is using it.
type parseCacheEntry struct {
	path string
	lock *os.File
}

// release drops the lock. It deliberately does NOT delete the db — persisting
// it across runs is the entire point.
func (e *parseCacheEntry) release() {
	if e == nil || e.lock == nil {
		return
	}
	_ = syscall.Flock(int(e.lock.Fd()), syscall.LOCK_UN)
	_ = e.lock.Close()
}

// discard deletes the db and everything leyline writes beside it, then
// releases the lock. For a parse that failed and may have left the db
// half-written — it would otherwise be the INPUT to every later run, so a
// single bad parse would poison the cache indefinitely.
//
// The capnp sidecars share the db's stem rather than its full name
// (`<stem>.db` next to `<stem>.ast.capnp`), so removing only `path + suffix`
// would leave them behind to be read against a db that no longer matches.
func (e *parseCacheEntry) discard() {
	if e == nil {
		return
	}
	stem := strings.TrimSuffix(e.path, ".db")
	for _, name := range []string{
		e.path, e.path + "-wal", e.path + "-shm",
		stem + ".ast.capnp", stem + ".head.capnp", stem + ".source.capnp",
	} {
		_ = os.Remove(name)
	}
	e.release()
}

// reserveParseCache returns the persistent parse db for sourceDir, or nil when
// the cache is unavailable — in which case the caller falls back to a private
// temp db, which is what mache did unconditionally before mache-80a851.
//
// Reusing the output path is the whole mechanism: leyline diffs the tree
// against what the db already holds and re-parses only what changed. Pointed
// at a fresh temp file it has nothing to diff and re-parses everything, which
// on mache itself is 16.6 s against 42 ms for the same unchanged tree.
//
// The db is keyed by BOTH the resolved source root and the pinned leyline
// version. The source root because one shared db for every project is the
// failure mache-e3e19f records: a second tree hits leyline's warm-start
// refusal and daemon-backed features go dark. The version because a different
// leyline writes a different `_ast` schema, so a db from another version must
// miss rather than be read with the wrong shape.
func reserveParseCache(sourceDir string) *parseCacheEntry {
	dir, err := parseCacheDir()
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}

	root, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		root = sourceDir
	}
	if abs, aerr := filepath.Abs(root); aerr == nil {
		root = abs
	}
	sum := sha256.Sum256([]byte(root))
	base := fmt.Sprintf("%s-%s", hex.EncodeToString(sum[:8]), leyline.BinaryVersion)
	dbPath := filepath.Join(dir, base+".db")

	// One writer at a time. A second mache on the same project fails the lock
	// and takes the temp path — exactly the isolation every process had before
	// this cache existed — rather than re-parsing into a db another process is
	// serving from underneath it. The lock is held for the caller's whole
	// session, not just the parse, because `mache serve` keeps reading the db
	// long after leyline has exited.
	lock, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil
	}
	return &parseCacheEntry{path: dbPath, lock: lock}
}

// parseCacheDir is where persistent parse dbs live.
//
// Resolved through projcfg.MacheHomeDir rather than a hand-rolled
// filepath.Join(home, ".mache"), so it inherits that seam's test-hermeticity
// guard: under `go test` with the real HOME it returns an error, the cache is
// unavailable, and the caller silently gets the old temp-file behaviour. A
// test therefore cannot populate a developer's real cache (mache-3e78d2).
func parseCacheDir() (string, error) {
	if override := os.Getenv(ParseCacheDirEnv); override != "" {
		return override, nil
	}
	home, err := projcfg.MacheHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "parse"), nil
}
