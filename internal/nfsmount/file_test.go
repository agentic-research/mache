package nfsmount

import (
	"io"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- writeFile tests ---

func TestWriteFile_WriteAndClose_TriggersCallback(t *testing.T) {
	var gotID string
	var gotContent []byte
	var called bool

	f := &writeFile{
		id:     "funcs/Foo/source",
		origin: graph.SourceOrigin{FilePath: "foo.go", StartByte: 0, EndByte: 10},
		buf:    make([]byte, 0),
		onClose: func(nodeID string, origin graph.SourceOrigin, content []byte) error {
			called = true
			gotID = nodeID
			gotContent = append([]byte{}, content...) // copy
			return nil
		},
	}

	n, err := f.Write([]byte("new content"))
	require.NoError(t, err)
	assert.Equal(t, 11, n)

	err = f.Close()
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "funcs/Foo/source", gotID)
	assert.Equal(t, []byte("new content"), gotContent)
}

func TestWriteFile_TruncateOnly_DoesNotTriggerCallback(t *testing.T) {
	called := false
	f := &writeFile{
		id:  "funcs/Foo/source",
		buf: []byte("original"),
		onClose: func(string, graph.SourceOrigin, []byte) error {
			called = true
			return nil
		},
	}

	// SETATTR(size=0) cycle: Truncate + Close without Write
	err := f.Truncate(0)
	require.NoError(t, err)
	assert.Empty(t, f.buf)

	err = f.Close()
	require.NoError(t, err)
	assert.False(t, called, "onClose must NOT fire on truncate-only close")
}

func TestWriteFile_WriteAfterTruncate_TriggersCallback(t *testing.T) {
	var gotContent []byte
	f := &writeFile{
		id:  "funcs/Foo/source",
		buf: []byte("original"),
		onClose: func(_ string, _ graph.SourceOrigin, content []byte) error {
			gotContent = append([]byte{}, content...)
			return nil
		},
	}

	require.NoError(t, f.Truncate(0))
	_, err := f.Write([]byte("replaced"))
	require.NoError(t, err)

	require.NoError(t, f.Close())
	assert.Equal(t, []byte("replaced"), gotContent)
}

func TestWriteFile_BufferGrows(t *testing.T) {
	f := &writeFile{
		id:  "test",
		buf: []byte("ab"),
	}

	// Write past current length
	f.pos = 2
	n, err := f.Write([]byte("cdef"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte("abcdef"), f.buf)
}

// --- bytesFile tests ---

func TestBytesFile_Read(t *testing.T) {
	f := &bytesFile{name: "test", data: []byte("hello world")}

	buf := make([]byte, 5)
	n, err := f.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "hello", string(buf))

	// Second read continues from position
	n, err = f.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, " worl", string(buf))

	// Final read hits EOF
	n, err = f.Read(buf)
	assert.Equal(t, io.EOF, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, byte('d'), buf[0])
}

func TestBytesFile_ReadAt(t *testing.T) {
	f := &bytesFile{name: "test", data: []byte("hello world")}

	buf := make([]byte, 5)
	n, err := f.ReadAt(buf, 6)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "world", string(buf))

	// ReadAt past end
	_, err = f.ReadAt(buf, 100)
	assert.Equal(t, io.EOF, err)
}

func TestBytesFile_Seek(t *testing.T) {
	f := &bytesFile{name: "test", data: []byte("hello")}

	// SeekStart
	pos, err := f.Seek(3, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)

	// SeekCurrent
	pos, err = f.Seek(1, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(4), pos)

	// SeekEnd
	pos, err = f.Seek(-2, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)

	// Negative clamps to 0
	pos, err = f.Seek(-100, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pos)
}

func TestBytesFile_ReadEmpty(t *testing.T) {
	f := &bytesFile{name: "empty", data: []byte{}}

	buf := make([]byte, 10)
	_, err := f.Read(buf)
	assert.Equal(t, io.EOF, err)
}
