package leylinegraph

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/internal/projcfg"
)

// Bounding mache's disk footprint (mache-8178a5).
//
// Two leaks, measured on a developer machine on 2026-09-25, together holding
// over 100 GB and filling a 926 GB volume to 1.8 GiB free — which broke
// `task check` outright with "no space left on device":
//
//	$TMPDIR/mache-leyline-*.db    7,247 files.  AutoInvokeLeylineParse falls
//	                              back to a temp db whenever the parse cache is
//	                              unavailable — which is EVERY `go test`, since
//	                              the hermeticity guard (mache-3e78d2)
//	                              deliberately refuses the real ~/.mache there.
//	                              Its cleanup is a returned closure, so a panic,
//	                              a failed assertion or a caller that forgets
//	                              leaks ~1.8 GB. Nothing ever swept them.
//
//	~/.mache/parse/*.db           64 GB across 38 entries. reserveParseCache
//	                              never removed anything, and the key includes
//	                              the source root, so every distinct tree mints
//	                              a permanent entry.
//
// Both are swept opportunistically at parse time: one readdir each, best
// effort, and a failure is logged rather than returned — a reaper must never
// be the reason a parse fails.

const (
	// TempDBMaxAgeEnv overrides how old an orphaned temp db must be before it
	// is reaped. Accepts any time.ParseDuration string. Tests set it; nothing
	// else should need to.
	TempDBMaxAgeEnv = "MACHE_TEMP_DB_MAX_AGE"

	// ParseCacheMaxBytesEnv overrides the parse cache's size budget, in bytes.
	ParseCacheMaxBytesEnv = "MACHE_PARSE_CACHE_MAX_BYTES"

	// defaultTempDBMaxAge is deliberately far longer than any parse. A parse of
	// mache itself takes ~15 s cold; a temp db a day old belongs to a process
	// that is gone. Being generous costs disk for a day and is the difference
	// between reaping garbage and reaping a slow colleague's work.
	defaultTempDBMaxAge = 24 * time.Hour

	// defaultParseCacheMaxBytes holds a handful of projects warm without the
	// cache becoming the largest thing in the home directory. One entry is
	// ~1.8 GB for a 1,000-file repo, so this is roughly five to six trees.
	defaultParseCacheMaxBytes int64 = 10 << 30 // 10 GiB

	tempDBPattern = "mache-leyline-*.db"
)

// entryFiles returns every path leyline writes for one db. The capnp sidecars
// share the db's STEM rather than its full name, so removing `path + suffix`
// alone leaves them behind — the bug parseCacheEntry.discard already documents.
func entryFiles(dbPath string) []string {
	stem := strings.TrimSuffix(dbPath, ".db")
	return []string{
		dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + ".lock",
		stem + ".ast.capnp", stem + ".head.capnp", stem + ".source.capnp",
	}
}

// entryBytes is the total size of one cache entry, sidecars included. A db is
// a fraction of what an entry costs on disk — the .ast.capnp beside it is
// frequently larger — so budgeting on the db alone would undercount badly.
func entryBytes(dbPath string) int64 {
	var total int64
	for _, f := range entryFiles(dbPath) {
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total
}

func removeEntry(dbPath string) {
	for _, f := range entryFiles(dbPath) {
		_ = os.Remove(f)
	}
}

// reapStaleTempDBs removes orphaned `mache-leyline-*.db` files, and their
// sidecars, that nothing has touched for longer than the max age.
//
// Returns how many entries it removed, for the tests and the log line.
func reapStaleTempDBs(dir string, maxAge time.Duration, now time.Time) int {
	matches, err := filepath.Glob(filepath.Join(dir, tempDBPattern))
	if err != nil {
		return 0
	}
	reaped := 0
	for _, db := range matches {
		fi, serr := os.Stat(db)
		if serr != nil || fi.IsDir() {
			continue
		}
		if now.Sub(fi.ModTime()) <= maxAge {
			continue
		}
		removeEntry(db)
		reaped++
	}
	return reaped
}

// evictParseCache enforces a byte budget over the parse cache, removing
// least-recently-used entries until the total fits.
//
// It only removes an entry whose lock it can take, so an entry another process
// is serving from is skipped rather than deleted underneath it — the same
// non-blocking flock reserveParseCache uses to decide ownership. An entry it
// cannot lock also does not count as reclaimable, so the loop cannot spin
// trying to free space that is pinned.
//
// Returns the bytes reclaimed.
func evictParseCache(dir string, maxBytes int64) int64 {
	if maxBytes <= 0 {
		return 0
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return 0
	}

	type evictable struct {
		path  string
		bytes int64
		used  time.Time
	}
	entries := make([]evictable, 0, len(matches))
	var total int64
	for _, db := range matches {
		fi, serr := os.Stat(db)
		if serr != nil || fi.IsDir() {
			continue
		}
		size := entryBytes(db)
		total += size
		entries = append(entries, evictable{path: db, bytes: size, used: fi.ModTime()})
	}
	if total <= maxBytes {
		return 0
	}

	// Oldest first: the entry least recently parsed into is the one whose next
	// hit is furthest away.
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })

	var freed int64
	for _, e := range entries {
		if total-freed <= maxBytes {
			break
		}
		lock, lerr := os.OpenFile(e.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if lerr != nil {
			continue
		}
		if ferr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
			_ = lock.Close() // in use by another mache; leave it alone
			continue
		}
		removeEntry(e.path)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		_ = os.Remove(e.path + ".lock")
		freed += e.bytes
	}
	return freed
}

// reapDisk runs both sweeps. Called for its side effect at parse time; every
// failure is swallowed because a full disk is bad but a parse that refuses to
// run because it could not tidy up is worse.
func reapDisk() {
	if n := reapStaleTempDBs(os.TempDir(),
		projcfg.EnvDurationOr(TempDBMaxAgeEnv, defaultTempDBMaxAge), time.Now()); n > 0 {
		log.Printf("reaped %d orphaned leyline temp db(s) from %s", n, os.TempDir())
	}
	dir, err := parseCacheDir()
	if err != nil {
		return
	}
	if freed := evictParseCache(dir,
		projcfg.EnvBytesOr(ParseCacheMaxBytesEnv, defaultParseCacheMaxBytes)); freed > 0 {
		log.Printf("parse cache over budget: evicted %.1f MiB", float64(freed)/(1<<20))
	}
}
