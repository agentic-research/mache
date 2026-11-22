package nfsmount

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// bytesFile and writeFile read and seek through the same three helpers, so
// the two types cannot drift apart on io.Reader, io.ReaderAt or io.Seeker.

func TestReadAdvancing(t *testing.T) {
	data, pos := []byte("hello"), int64(0)
	p := make([]byte, 3)

	n, err := readAdvancing(data, &pos, p)
	assert.Equal(t, 3, n)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), pos)

	n, err = readAdvancing(data, &pos, p)
	assert.Equal(t, 2, n)
	assert.ErrorIs(t, err, io.EOF, "the last bytes arrive with EOF")
	assert.Equal(t, "lo", string(p[:n]))

	n, err = readAdvancing(data, &pos, p)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)

	pos = 99
	n, err = readAdvancing(data, &pos, p)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF, "a position past the end reads nothing")
	assert.Equal(t, int64(99), pos)
}

func TestReadAtOffset(t *testing.T) {
	data := []byte("hello")

	p := make([]byte, 2)
	n, err := readAtOffset(data, p, 1)
	assert.Equal(t, "el", string(p[:n]))
	assert.NoError(t, err, "a filled buffer is not EOF")

	p = make([]byte, 8)
	n, err = readAtOffset(data, p, 3)
	assert.Equal(t, "lo", string(p[:n]))
	assert.ErrorIs(t, err, io.EOF, "a short read reports EOF")

	n, err = readAtOffset(data, p, 5)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestSeekWithin(t *testing.T) {
	pos := int64(2)
	assert.Equal(t, int64(4), seekWithin(&pos, 10, 4, io.SeekStart))
	assert.Equal(t, int64(7), seekWithin(&pos, 10, 3, io.SeekCurrent))
	assert.Equal(t, int64(8), seekWithin(&pos, 10, -2, io.SeekEnd))
	assert.Equal(t, int64(0), seekWithin(&pos, 10, -50, io.SeekCurrent), "a negative position clamps to 0")
	assert.Equal(t, int64(0), pos)
	assert.Equal(t, int64(15), seekWithin(&pos, 10, 5, io.SeekEnd), "seeking past the end is allowed")
	assert.Equal(t, int64(0), seekWithin(&pos, 10, 5, 42), "an unknown whence seeks to 0")
}

// Both file types answer through the helpers.
func TestByteFiles_ShareReadAndSeek(t *testing.T) {
	b := &bytesFile{data: []byte("abcdef")}
	w := &writeFile{buf: []byte("abcdef")}
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
