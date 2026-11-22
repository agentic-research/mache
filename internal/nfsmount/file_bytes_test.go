package nfsmount

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// bytesFile and writeFile both embed sliceFile, so the two types cannot
// drift apart on io.Reader, io.ReaderAt or io.Seeker.

func TestSliceFile_Read(t *testing.T) {
	f := &sliceFile{data: []byte("hello")}
	p := make([]byte, 3)

	n, err := f.Read(p)
	assert.Equal(t, 3, n)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), f.pos)

	n, err = f.Read(p)
	assert.Equal(t, "lo", string(p[:n]))
	assert.ErrorIs(t, err, io.EOF, "the last bytes arrive with EOF")

	n, err = f.Read(p)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)

	f.pos = 99
	n, err = f.Read(p)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF, "a position past the end reads nothing")
	assert.Equal(t, int64(99), f.pos)
}

func TestSliceFile_ReadAt(t *testing.T) {
	f := &sliceFile{data: []byte("hello"), pos: 4}

	p := make([]byte, 2)
	n, err := f.ReadAt(p, 1)
	assert.Equal(t, "el", string(p[:n]))
	assert.NoError(t, err, "a filled buffer is not EOF")
	assert.Equal(t, int64(4), f.pos, "ReadAt does not move the position")

	p = make([]byte, 8)
	n, err = f.ReadAt(p, 3)
	assert.Equal(t, "lo", string(p[:n]))
	assert.ErrorIs(t, err, io.EOF, "a short read reports EOF")

	n, err = f.ReadAt(p, 5)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestSliceFile_Seek(t *testing.T) {
	f := &sliceFile{data: make([]byte, 10), pos: 2}
	seek := func(offset int64, whence int) int64 {
		pos, err := f.Seek(offset, whence)
		assert.NoError(t, err)
		assert.Equal(t, pos, f.pos)
		return pos
	}
	assert.Equal(t, int64(4), seek(4, io.SeekStart))
	assert.Equal(t, int64(7), seek(3, io.SeekCurrent))
	assert.Equal(t, int64(8), seek(-2, io.SeekEnd))
	assert.Equal(t, int64(0), seek(-50, io.SeekCurrent), "a negative position clamps to 0")
	assert.Equal(t, int64(15), seek(5, io.SeekEnd), "seeking past the end is allowed")
	assert.Equal(t, int64(0), seek(5, 42), "an unknown whence seeks to 0")
}

// Both file types answer through the one implementation.
func TestByteFiles_ShareReadAndSeek(t *testing.T) {
	b := &bytesFile{sliceFile: sliceFile{data: []byte("abcdef")}}
	w := &writeFile{sliceFile: sliceFile{data: []byte("abcdef")}}
	for _, f := range []interface {
		io.Reader
		io.ReaderAt
		io.Seeker
	}{b, w} {
		pos, err := f.Seek(-2, io.SeekEnd)
		assert.NoError(t, err)
		assert.Equal(t, int64(4), pos)
		p := make([]byte, 4)
		n, err := f.Read(p)
		assert.Equal(t, "ef", string(p[:n]))
		assert.ErrorIs(t, err, io.EOF)
		n, err = f.ReadAt(p, 1)
		assert.Equal(t, "bcde", string(p[:n]))
		assert.NoError(t, err)
	}
}
