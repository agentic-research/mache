package nfsmount

import (
	"fmt"
	"io"

	billy "github.com/go-git/go-billy/v5"

	"github.com/agentic-research/mache/internal/graph"
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
	data []byte
	pos  int64
}

func (f *bytesFile) Name() string { return f.name }

func (f *bytesFile) Read(p []byte) (int, error) {
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

func (f *bytesFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *bytesFile) Seek(offset int64, whence int) (int64, error) {
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

func (f *bytesFile) Write([]byte) (int, error) { return 0, errReadOnly }
func (f *bytesFile) Truncate(int64) error      { return errReadOnly }
func (f *bytesFile) Lock() error               { return nil }
func (f *bytesFile) Unlock() error             { return nil }
func (f *bytesFile) Close() error              { return nil }

// writeFile implements billy.File with buffered writes and splice-on-Close.
// NFS WRITE RPCs arrive as individual writes; we buffer them all and commit
// the final content when the file is closed (NFS CLOSE / billy Close).
type writeFile struct {
	id      string
	origin  graph.SourceOrigin
	buf     []byte
	pos     int64
	written bool // true only when Write() has been called (not just Truncate)
	onClose WriteBackFunc
}

func (f *writeFile) Name() string { return f.id }

func (f *writeFile) Read(p []byte) (int, error) {
	if f.pos >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[f.pos:])
	f.pos += int64(n)
	if f.pos >= int64(len(f.buf)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *writeFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *writeFile) Write(p []byte) (int, error) {
	end := f.pos + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	n := copy(f.buf[f.pos:], p)
	f.pos += int64(n)
	f.written = true
	return n, nil
}

func (f *writeFile) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = f.pos + offset
	case io.SeekEnd:
		newPos = int64(len(f.buf)) + offset
	}
	if newPos < 0 {
		newPos = 0
	}
	f.pos = newPos
	return f.pos, nil
}

func (f *writeFile) Truncate(size int64) error {
	if size < int64(len(f.buf)) {
		f.buf = f.buf[:size]
	} else if size > int64(len(f.buf)) {
		grown := make([]byte, size)
		copy(grown, f.buf)
		f.buf = grown
	}
	// Note: Truncate alone does NOT mark written. NFS SETATTR(size=0) causes
	// a Truncate+Close cycle before WRITE — splicing on truncate-only would
	// delete source content. Only Write() sets written=true.
	return nil
}

// Close is THE COMMIT POINT. Only splice if Write() was actually called.
// NFS SETATTR(size=0) causes a Truncate+Close cycle without Write — we
// must NOT splice in that case, or the source file content gets deleted.
func (f *writeFile) Close() error {
	if !f.written || f.onClose == nil {
		return nil
	}
	if err := f.onClose(f.id, f.origin, f.buf); err != nil {
		return fmt.Errorf("write-back failed for %s: %w", f.id, err)
	}
	return nil
}

func (f *writeFile) Lock() error   { return nil }
func (f *writeFile) Unlock() error { return nil }

var _ billy.File = (*writeFile)(nil)
