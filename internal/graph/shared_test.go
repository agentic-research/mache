package graph

import (
	"fmt"
	"sync"
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

func TestSliceContent_NegativeOffset(t *testing.T) {
	data := []byte("hello")
	buf := make([]byte, 10)
	assert.Equal(t, 0, SliceContent(data, buf, -1))
	assert.Equal(t, 0, SliceContent(data, buf, -9999))
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

// ---------------------------------------------------------------------------
// ContentCache — FIFO-bounded content cache with RWMutex
// ---------------------------------------------------------------------------

func TestContentCache_GetPut(t *testing.T) {
	c := NewContentCache(10)
	c.Put("a", []byte("hello"))

	v, ok := c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, []byte("hello"), v)
}

func TestContentCache_GetMiss(t *testing.T) {
	c := NewContentCache(10)
	_, ok := c.Get("missing")
	assert.False(t, ok)
}

func TestContentCache_PutDedup(t *testing.T) {
	c := NewContentCache(10)
	c.Put("a", []byte("v1"))
	c.Put("a", []byte("v2")) // update, not duplicate entry

	v, ok := c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, []byte("v2"), v)

	// Keys should have only one entry for "a"
	c.mu.RLock()
	count := 0
	for _, k := range c.keys {
		if k == "a" {
			count++
		}
	}
	c.mu.RUnlock()
	assert.Equal(t, 1, count, "duplicate key 'a' in keys slice")
}

func TestContentCache_FIFO_Eviction(t *testing.T) {
	c := NewContentCache(3)
	c.Put("a", []byte("1"))
	c.Put("b", []byte("2"))
	c.Put("c", []byte("3"))

	// Cache full — putting "d" should evict "a" (FIFO)
	c.Put("d", []byte("4"))

	_, ok := c.Get("a")
	assert.False(t, ok, "'a' should have been evicted")

	v, ok := c.Get("d")
	assert.True(t, ok)
	assert.Equal(t, []byte("4"), v)

	// b and c should still be present
	_, ok = c.Get("b")
	assert.True(t, ok)
	_, ok = c.Get("c")
	assert.True(t, ok)
}

func TestContentCache_Delete(t *testing.T) {
	c := NewContentCache(10)
	c.Put("a", []byte("1"))
	c.Put("b", []byte("2"))
	c.Put("c", []byte("3"))

	c.Delete("b")

	_, ok := c.Get("b")
	assert.False(t, ok, "'b' should be deleted")

	// a and c still present
	_, ok = c.Get("a")
	assert.True(t, ok)
	_, ok = c.Get("c")
	assert.True(t, ok)
}

func TestContentCache_Delete_NoPhantomEviction(t *testing.T) {
	// This test catches the bug in SQLiteGraph/WritableGraph where
	// Invalidate deletes from map but not from keys, causing phantom
	// eviction entries that shrink effective cache capacity.
	c := NewContentCache(3)
	c.Put("a", []byte("1"))
	c.Put("b", []byte("2"))
	c.Put("c", []byte("3"))

	// Delete "b" — should remove from BOTH map and keys
	c.Delete("b")

	// Now we have {a, c} with cap 3. Fill two more:
	c.Put("d", []byte("4")) // {a, c, d} — full
	c.Put("e", []byte("5")) // evicts "a" (oldest) → {c, d, e}

	// If Delete left a phantom "b" in keys, the keys slice would be
	// [a, b, c, d] after Put("d"), and Put("e") would evict "a" (correct)
	// but then Put("f") would evict phantom "b" (no-op delete from map)
	// and effective capacity drops to 2.
	c.Put("f", []byte("6")) // evicts "c" → {d, e, f}

	// All three should be present — if phantoms exist, one would be evicted
	_, ok := c.Get("d")
	assert.True(t, ok, "'d' evicted — phantom entry in keys")
	_, ok = c.Get("e")
	assert.True(t, ok, "'e' evicted — phantom entry in keys")
	_, ok = c.Get("f")
	assert.True(t, ok, "'f' evicted — phantom entry in keys")

	// Verify keys slice length matches map size
	c.mu.RLock()
	assert.Equal(t, len(c.entries), len(c.keys),
		"keys slice length (%d) != entries map size (%d)", len(c.keys), len(c.entries))
	c.mu.RUnlock()
}

func TestContentCache_Delete_NonExistent(t *testing.T) {
	c := NewContentCache(10)
	c.Put("a", []byte("1"))

	// Should not panic
	c.Delete("nonexistent")

	v, ok := c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, []byte("1"), v)
}

func TestContentCache_ConcurrentReadWrite(t *testing.T) {
	c := NewContentCache(100)
	var wg sync.WaitGroup

	// Writers
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				key := fmt.Sprintf("key_%d_%d", i, j)
				c.Put(key, []byte(key))
			}
		}()
	}

	// Readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Get("key_0_0") // may or may not exist
			}
		}()
	}

	// Deleters
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				c.Delete(fmt.Sprintf("key_0_%d", j))
			}
		}()
	}

	wg.Wait()
}
