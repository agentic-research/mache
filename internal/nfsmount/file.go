package nfsmount

import (
	"fmt"
	"io"

	billy "github.com/go-git/go-billy/v5"

	"github.com/agentic-research/mache/graph"
)

// WriteBackFunc is the callback triggered when a writable file is closed.
// It receives the node ID, the source origin, and the new content.
type WriteBackFunc func(nodeID string, origin graph.SourceOrigin, content []byte) error

// graphFile implements billy.File backed by graph.ReadContent.
// Read-only: Write and Truncate return errors.
type graphFile struct {
	id    string
	size  int64
	graph graph.Graph
	pos   int64
}

func (f *graphFile) Name() string { return f.id }

func (f *graphFile) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	n, err := f.graph.ReadContent(f.id, p, f.pos)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	f.pos += int64(n)
	if f.pos >= f.size {
		return n, io.EOF
	}
	return n, nil
}

func (f *graphFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.size {
		return 0, io.EOF
	}
	n, err := f.graph.ReadContent(f.id, p, off)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *graphFile) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = f.pos + offset
	case io.SeekEnd:
		newPos = f.size + offset
	}
	if newPos < 0 {
		newPos = 0
	}
	f.pos = newPos
	return f.pos, nil
}

func (f *graphFile) Write([]byte) (int, error) { return 0, errReadOnly }
func (f *graphFile) Truncate(int64) error      { return errReadOnly }
func (f *graphFile) Lock() error               { return nil }
func (f *graphFile) Unlock() error             { return nil }
func (f *graphFile) Close() error              { return nil }

// bytesFile implements billy.File backed by a static byte slice.
// Used for virtual files like _schema.json.
type bytesFile struct {
	name string
	sliceFile
}

func (f *bytesFile) Name() string { return f.name }

func (f *bytesFile) Write([]byte) (int, error) { return 0, errReadOnly }
func (f *bytesFile) Truncate(int64) error      { return errReadOnly }
func (f *bytesFile) Lock() error               { return nil }
func (f *bytesFile) Unlock() error             { return nil }
func (f *bytesFile) Close() error              { return nil }

// writeFile implements billy.File with buffered writes and splice-on-Close.
// NFS WRITE RPCs arrive as individual writes; we buffer them all and commit
// the final content when the file is closed (NFS CLOSE / billy Close).
type writeFile struct {
	id        string
	origin    graph.SourceOrigin
	sliceFile      // the buffered content and the client's position in it
	written   bool // true only when Write() has been called (not just Truncate)
	onClose   WriteBackFunc

	// replaceAtZero makes a first write at offset 0 replace the whole buffer
	// (see openWritable). truncated is set by an explicit Truncate, after which
	// the buffer is already what the client meant.
	replaceAtZero, truncated bool
	// session, when set, records the client's bytes on Close so the next
	// WRITE request, which go-nfs serves as a fresh open, continues them.
	session *GraphFS
}

func (f *writeFile) Name() string { return f.id }

func (f *writeFile) Write(p []byte) (int, error) {
	if !f.written && !f.truncated && f.replaceAtZero && f.pos == 0 {
		f.data = f.data[:0]
	}
	end := f.pos + int64(len(p))
	if end > int64(len(f.data)) {
		grown := make([]byte, end)
		copy(grown, f.data)
		f.data = grown
	}
	n := copy(f.data[f.pos:], p)
	f.pos += int64(n)
	f.written = true
	return n, nil
}

func (f *writeFile) Truncate(size int64) error {
	if size < int64(len(f.data)) {
		f.data = f.data[:size]
	} else if size > int64(len(f.data)) {
		grown := make([]byte, size)
		copy(grown, f.data)
		f.data = grown
	}
	// Truncate alone does NOT set written — see Close() comment.
	f.truncated = true
	return nil
}

// Close is THE COMMIT POINT. Only splice if Write() was actually called.
// NFS SETATTR(size=0) causes a Truncate+Close cycle without Write — we
// must NOT splice in that case, or the source file content gets deleted.
//
// Either way the client's bytes are recorded in the write session, so a
// truncate is not forgotten by the WRITE that follows it, and a later chunk
// of the same edit continues from them (mache-bc65f8).
func (f *writeFile) Close() error {
	if !f.written || f.onClose == nil {
		if f.truncated && f.session != nil {
			f.session.saveSession(f.id, f.data)
		}
		return nil
	}
	err := f.onClose(f.id, f.origin, f.data)
	if f.session != nil {
		f.session.saveSession(f.id, f.data)
	}
	if err != nil {
		return fmt.Errorf("write-back failed for %s: %w", f.id, err)
	}
	return nil
}

func (f *writeFile) Lock() error   { return nil }
func (f *writeFile) Unlock() error { return nil }

var _ billy.File = (*writeFile)(nil)

// sliceFile is a byte slice read as a file: the io.Reader, io.ReaderAt and
// io.Seeker that bytesFile and writeFile both are. They embed it, so the two
// cannot drift apart on how a read ends or a seek clamps; each used to carry
// its own copy of all three methods.
type sliceFile struct {
	data []byte
	pos  int64
}

// Read copies from the current position and advances it, returning io.EOF
// with the last bytes as well as after them.
func (f *sliceFile) Read(p []byte) (int, error) {
	if f.pos >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += int64(n)
	if f.pos >= int64(len(f.data)) {
		return n, io.EOF
	}
	return n, nil
}

// ReadAt copies from off without moving the position, returning io.EOF when
// p was not filled.
func (f *sliceFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Seek moves the position as io.Seeker describes, clamping a negative result
// to 0. An unknown whence seeks to 0.
func (f *sliceFile) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = f.pos + offset
	case io.SeekEnd:
		newPos = int64(len(f.data)) + offset
	}
	if newPos < 0 {
		newPos = 0
	}
	f.pos = newPos
	return f.pos, nil
}
