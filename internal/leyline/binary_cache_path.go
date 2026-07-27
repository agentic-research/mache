package leyline

import (
	"fmt"
	"os"
	"path/filepath"
)

// The leyline binary cache is namespaced BY PINNED VERSION.
//
// It used to be a single unversioned path, ~/.mache/bin/leyline, and every
// mache build on a machine treated that one file as "its" cache. Each build is
// individually correct — find a binary that is not my pin, replace it with one
// that is — but together they thrash: an installed mache pinning v0.10.2 and a
// working branch pinning v0.10.3 overwrite each other on every invocation.
//
// Observed 2026-07-27: ~/.local/bin/mache (commit 1c3d812, pin v0.10.2) and this
// branch (pin v0.10.3) traded the file back and forth across four consecutive
// `task check` runs. Projections and a smell baseline were generated in between
// without any indication of which producer had won, and LLO ships _ast schema
// changes in PATCH releases — so the graph shape silently depended on which
// mache last touched the cache.
//
// Namespacing removes the shared mutable resource rather than guarding it.
// Concurrent pins now coexist: ~/.mache/bin/leyline-v0.10.2 and
// ~/.mache/bin/leyline-v0.10.3 are different files, and neither build has any
// reason to touch the other's. This makes the whole class impossible instead of
// merely visible, which the MACHE_LEYLINE_BINARY override and leyline:doctor
// only achieved.
//
// Related: mache-0acdf6 ("the ~/.mache/bin fallback rots into version skew") —
// this is the mechanism behind that bead, and mache-608a3c, which tightened the
// version gate that was working correctly the whole time.

// cacheDir is where pinned leyline binaries live.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".mache", "bin"), nil
}

// pinnedCachePath is the version-namespaced cache location for THIS build's
// pin. Two mache builds with different pins resolve to different files, so
// neither can invalidate the other.
func pinnedCachePath() (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "leyline-"+leylineBinaryVersion), nil
}

// legacyCachePath is the pre-namespacing location. It is still READ — if it
// happens to hold this build's pin there is no reason to re-download — but it
// is never WRITTEN, so a build with a different pin can no longer clobber it
// and the contention ends after one migration pass.
func legacyCachePath() (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "leyline"), nil
}

// resolveCachedPinned returns a cached binary matching this build's pin,
// preferring the namespaced path and falling back to the legacy unversioned one.
// Returns "" when neither matches; callers download to the namespaced path.
func resolveCachedPinned() string {
	if p, err := pinnedCachePath(); err == nil {
		if _, statErr := os.Stat(p); statErr == nil && leylineVersionMatchesPin(p) {
			return p
		}
	}
	// Legacy path: usable only when it already IS the pin. A mismatch here is
	// no longer an error worth acting on — it belongs to whichever build wrote
	// it, and we simply use our own namespaced copy instead.
	if p, err := legacyCachePath(); err == nil {
		if _, statErr := os.Stat(p); statErr == nil && leylineVersionMatchesPin(p) {
			return p
		}
	}
	return ""
}
