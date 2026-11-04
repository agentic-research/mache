//go:build unix

package lltest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// WritableTree is a source tree set up the way a writable mount or serve has
// it (mache-3a0fb9): parsed by the pinned leyline, with mache's managed daemon
// spawned from the pinned binary on the tree as its --source, so a write
// proposes through it.
type WritableTree struct {
	// Blobs is the parse db's BlobSource, as leylinegraph.WireParseDB gives
	// a store.
	Blobs graph.BlobSource
	db    *sql.DB
	root  string
}

// UseWritableTree parses dir and points mache's managed daemon at it, under a
// private HOME, with the pinned binary named by the override (so the daemon is
// one this process spawns, serving dir, so WritableDaemon accepts it). The daemon is
// stopped at cleanup. Skips when the pinned binary is not cached.
func UseWritableTree(t *testing.T, dir string) *WritableTree {
	t.Helper()
	bin := ResolveBinaryOrSkip(t).Path
	db, root := ParseSourceViaLeyline(t, dir)
	home, err := os.MkdirTemp("/tmp", "llw") // short: the socket lives under it
	if err != nil {
		t.Fatalf("lltest: writable tree home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("MACHE_NO_LEYLINE", "")
	t.Setenv(leyline.BinaryOverrideEnv, bin)
	leyline.SetDaemonSource(root)
	t.Cleanup(func() {
		leyline.StopManaged()
		leyline.SetDaemonSource("")
	})
	return &WritableTree{Blobs: graph.NewParseBlobs(db), db: db, root: root}
}

// Blob is the parsed blob of the file at path: the base a write to a node in
// it names (SourceOrigin.Blob).
func (w *WritableTree) Blob(t *testing.T, path string) []byte {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("lltest: %v", err)
	}
	var hash []byte
	if err := w.db.QueryRow(`SELECT content_hash FROM _source WHERE path = ? OR path = ?`,
		abs, leyline.CanonicalSourceRoot(abs)).Scan(&hash); err != nil {
		t.Fatalf("lltest: no parsed blob for %s: %v", path, err)
	}
	return hash
}
