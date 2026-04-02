package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// NormalizeID — exported path normalization
// ---------------------------------------------------------------------------

func TestNormalizeID_StripLeadingSlash(t *testing.T) {
	assert.Equal(t, "vulns/CVE-1", NormalizeID("/vulns/CVE-1"))
}

func TestNormalizeID_NoSlash(t *testing.T) {
	assert.Equal(t, "vulns/CVE-1", NormalizeID("vulns/CVE-1"))
}

func TestNormalizeID_Empty(t *testing.T) {
	assert.Equal(t, "", NormalizeID(""))
}

func TestNormalizeID_JustSlash(t *testing.T) {
	assert.Equal(t, "", NormalizeID("/"))
}

func TestNormalizeID_DoubleSlash(t *testing.T) {
	// Only strips one leading slash — this matches the existing behavior
	assert.Equal(t, "/double", NormalizeID("//double"))
}

// ---------------------------------------------------------------------------
// SliceContent — byte-range copy for ReadContent implementations
// ---------------------------------------------------------------------------

func TestSliceContent_Full(t *testing.T) {
	data := []byte("hello world")
	buf := make([]byte, 20)
	n := SliceContent(data, buf, 0)
	assert.Equal(t, 11, n)
	assert.Equal(t, "hello world", string(buf[:n]))
}

func TestSliceContent_Offset(t *testing.T) {
	data := []byte("hello world")
	buf := make([]byte, 5)
	n := SliceContent(data, buf, 6)
	assert.Equal(t, 5, n)
	assert.Equal(t, "world", string(buf[:n]))
}

func TestSliceContent_OffsetBeyondEnd(t *testing.T) {
	data := []byte("hello")
	buf := make([]byte, 10)
	n := SliceContent(data, buf, 100)
	assert.Equal(t, 0, n)
}

func TestSliceContent_PartialRead(t *testing.T) {
	data := []byte("hello world")
	buf := make([]byte, 3)
	n := SliceContent(data, buf, 0)
	assert.Equal(t, 3, n)
	assert.Equal(t, "hel", string(buf[:n]))
}

func TestSliceContent_EmptyData(t *testing.T) {
	buf := make([]byte, 10)
	assert.Equal(t, 0, SliceContent(nil, buf, 0))
	assert.Equal(t, 0, SliceContent([]byte{}, buf, 0))
}

func TestSliceContent_EmptyBuf(t *testing.T) {
	data := []byte("hello")
	assert.Equal(t, 0, SliceContent(data, nil, 0))
	assert.Equal(t, 0, SliceContent(data, []byte{}, 0))
}

func TestSliceContent_ExactFit(t *testing.T) {
	data := []byte("abc")
	buf := make([]byte, 3)
	n := SliceContent(data, buf, 0)
	assert.Equal(t, 3, n)
	assert.Equal(t, "abc", string(buf[:n]))
}
