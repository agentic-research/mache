// Package machehome is the one place mache resolves its home directory,
// ~/.mache, or wherever MACHE_HOME points. Every path mache keeps per-machine
// state under (the project registry, parse cache, leyline arena, control
// block, sockets and owner records) goes through Dir; the pinned leyline
// binaries go through BinDir.
//
// One resolver is what makes the override and the test guard complete. When
// each site joined $HOME/.mache itself, only the project registry refused a
// test's write to the real directory, and tests and `task ci` filled a
// developer's ~/.mache with fixture projects and parse caches (mache-6d5e62).
package machehome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Env names the override: an absolute directory used in place of ~/.mache.
const Env = "MACHE_HOME"

// realDefault is the developer's ~/.mache: $HOME/.mache as the process saw it
// at start, before any test's t.Setenv can have run, whatever MACHE_HOME
// says. The guard in Dir compares against it.
var realDefault = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mache")
}()

// resolve is MACHE_HOME when set, else $HOME/.mache.
func resolve() (string, error) {
	if v := os.Getenv(Env); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s=%q must be an absolute path", Env, v)
		}
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".mache"), nil
}

// Dir is the directory mache keeps per-machine state under. It is not
// created here.
//
// Under `go test` it refuses the developer's real ~/.mache: a test that
// reaches it must first point MACHE_HOME (or HOME) at a t.TempDir(). It fails
// loudly, naming the fix, instead of quietly writing into the user's state.
// testing.Testing() is false in a production binary, so there the branch is
// dead.
func Dir() (string, error) {
	dir, err := resolve()
	if err != nil {
		return "", err
	}
	if testing.Testing() && realDefault != "" && dir == realDefault {
		return "", fmt.Errorf("test would touch the REAL %s — point %s (or HOME) at a t.TempDir() with t.Setenv first", dir, Env)
	}
	return dir, nil
}

// BinDir is where the pinned leyline binaries are cached: Dir's bin/. Unlike
// Dir it is not guarded under test. The cache is shared, version-named and
// read-only to tests, which resolve the pinned leyline from it.
func BinDir() (string, error) {
	dir, err := resolve()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bin"), nil
}
