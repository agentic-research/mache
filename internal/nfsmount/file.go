package nfsmount

import (
	"io"

	"github.com/agentic-research/mache/internal/graph"
)

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
