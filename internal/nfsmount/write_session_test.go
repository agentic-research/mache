package nfsmount

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// go-nfs v0.0.4 serves EVERY NFS WRITE request as OpenFile(O_RDWR) + Seek +
// Write + Close (nfs_onwrite.go), replies FILE_SYNC so the client never sends
// COMMIT, and serves SETATTR(size) as OpenFile + Truncate + Close. These tests
// drive GraphFS exactly that way (mache-bc65f8).

const sourceID = "/pkg/functions/Foo/source"

// writeRig is a writable GraphFS over one source node, whose write-back stores
// what it is given, after an optional formatter, as a real mount does.
type writeRig struct {
	t       *testing.T
	store   *graph.MemoryStore
	gfs     *GraphFS
	commits [][]byte
	format  func([]byte) []byte
}

func newWriteRig(t *testing.T, content string) *writeRig {
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{ID: "pkg", Mode: os.ModeDir, Children: []string{"pkg/functions"}})
	store.AddNode(&graph.Node{ID: "pkg/functions", Mode: os.ModeDir, Children: []string{"pkg/functions/Foo"}})
	store.AddNode(&graph.Node{ID: "pkg/functions/Foo", Mode: os.ModeDir, Children: []string{"pkg/functions/Foo/source"}})
	store.AddNode(&graph.Node{
		ID: "pkg/functions/Foo/source", Data: []byte(content),
		Origin: &graph.SourceOrigin{FilePath: "/src/foo.go", StartByte: 0, EndByte: uint32(len(content))},
	})
	r := &writeRig{t: t, store: store, gfs: NewGraphFS(store, newTestSchema())}
	r.gfs.SetWriteBack(func(id string, origin graph.SourceOrigin, content []byte) error {
		r.commits = append(r.commits, bytes.Clone(content))
		stored := bytes.Clone(content)
		if r.format != nil {
			stored = r.format(stored)
		}
		return store.UpdateNodeContent(strings.TrimPrefix(id, "/"), stored, nil, time.Now())
	})
	return r
}

// write is one NFS WRITE request, as go-nfs serves it.
func (r *writeRig) write(offset int64, data string) {
	r.t.Helper()
	f, err := r.gfs.OpenFile(sourceID, os.O_RDWR, 0o644)
	require.NoError(r.t, err)
	if offset > 0 {
		_, err = f.Seek(offset, io.SeekStart)
		require.NoError(r.t, err)
	}
	_, err = f.Write([]byte(data))
	require.NoError(r.t, err)
	require.NoError(r.t, f.Close())
}

// setattrSize is one NFS SETATTR(size), as go-nfs serves it.
func (r *writeRig) setattrSize(size int64) {
	r.t.Helper()
	f, err := r.gfs.OpenFile(sourceID, os.O_RDWR, 0o644)
	require.NoError(r.t, err)
	require.NoError(r.t, f.Truncate(size))
	require.NoError(r.t, f.Close())
}

func (r *writeRig) stored() string {
	r.t.Helper()
	buf := make([]byte, 4096)
	n, _ := r.store.ReadContent("pkg/functions/Foo/source", buf, 0)
	return string(buf[:n])
}

func (r *writeRig) last() string {
	r.t.Helper()
	require.NotEmpty(r.t, r.commits, "nothing was committed")
	return string(r.commits[len(r.commits)-1])
}

func noZeros(t *testing.T, commits [][]byte) {
	t.Helper()
	for i, c := range commits {
		assert.NotContains(t, string(c), "\x00", "commit %d carries zero bytes the client never wrote", i)
	}
}

const edit = "func Foo() int {\n\treturn 2\n}\n"

// The reported bug: a client that flushes one edit as two WRITEs. The second
// request used to open a truncated, empty buffer, seek past its end, and
// commit N zero bytes plus the second chunk over the construct.
func TestNFSWrite_ASplitWriteCommitsTheWholeEdit(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 }\n")
	r.write(0, edit[:10])
	r.write(10, edit[10:])

	noZeros(t, r.commits)
	assert.Equal(t, edit, r.last(), "the final commit must be the whole edit")
	assert.Equal(t, edit, r.stored())
}

// Write-back formats what it stores, so the node's bytes are not the client's.
// The client's next WRITE is an offset into ITS bytes, so it must continue
// from them, not from the formatted content.
func TestNFSWrite_ContinuesFromTheClientsBytesNotTheFormattedOnes(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 }\n")
	r.format = func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\t"), []byte("    ")) }
	r.write(0, edit[:20])
	r.write(20, edit[20:])

	assert.Equal(t, edit, r.last(), "chunk two lands at offset 20 of what the client wrote")
}

// An editor that truncates first (open O_TRUNC is SETATTR size=0 over NFS),
// then writes in chunks. The truncate alone must not splice (it would delete
// the construct), but it must not be forgotten either.
func TestNFSWrite_TruncateThenChunks(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 } // a much longer original body\n")
	r.setattrSize(0)
	assert.Empty(t, r.commits, "a truncate alone must not splice")
	r.write(0, edit[:12])
	r.write(12, edit[12:])

	noZeros(t, r.commits)
	assert.Equal(t, edit, r.last())
}

// A single write at offset 0 without a truncate still replaces the whole
// source: the convenience the implicit truncation exists for, kept.
func TestNFSWrite_AOneShotWriteAtZeroStillReplaces(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 } // a much longer original body\n")
	r.write(0, edit)
	assert.Equal(t, edit, r.last(), "no old tail survives a one-shot replace")
}

// A write at an offset with no write in progress patches the current content
// in place, as POSIX would. It used to zero-fill everything before it.
func TestNFSWrite_APatchAtAnOffsetKeepsWhatIsAroundIt(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 }\n")
	r.write(24, "2")
	noZeros(t, r.commits)
	assert.Equal(t, "func Foo() int { return 2 }\n", r.last())
}

// A write session belongs to one logical write. If something else changes
// the node in the meantime (another writer, a re-ingest), the next write
// works on the new content, not on the stale bytes of an older edit.
func TestNFSWrite_ASessionIsDroppedWhenTheNodeChangesUnderIt(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 }\n")
	r.write(0, "func Foo() int { return 7 }\n")
	// The other writer renames the function: a byte the patch below does not
	// touch, so applying the patch to the stale session would visibly lose it.
	require.NoError(t, r.store.UpdateNodeContent("pkg/functions/Foo/source",
		[]byte("func Qux() int { return 7 }\n"), nil, time.Now()))

	r.write(24, "3")
	assert.Equal(t, "func Qux() int { return 3 }\n", r.last(),
		"the patch applies to the content another writer left, not the stale session")
}

// A truncate to a non-zero size, then a write at that size: an append after
// shortening. The truncate arrives as its own SETATTR request and splices
// nothing, so unless it is remembered, the write lands on the full original
// and leaves its old tail behind.
func TestNFSWrite_ATruncateIsKeptForTheWriteAfterIt(t *testing.T) {
	r := newWriteRig(t, "func Foo() int { return 1 }\n")
	r.setattrSize(17) // "func Foo() int { "
	r.write(17, "x }\n")
	assert.Equal(t, "func Foo() int { x }\n", r.last(), "the old tail must not survive the truncate")
}
