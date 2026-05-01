package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The arena writer's higher-level coalescing + flip-buffer
// behavior is covered by arena_writer_test.go. These tests
// pin the lower-level header serialization + offset arithmetic
// directly — both pieces are reachable independently from
// callers that just want to read or validate an arena file
// without spinning up a full ArenaFlusher.

func TestArenaHeader_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "header.bin")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	want := &ArenaHeader{
		Magic:        ArenaMagic,
		Version:      1,
		ActiveBuffer: 1,
		Sequence:     42,
	}
	require.NoError(t, WriteArenaHeader(f, want))

	got, err := ReadArenaHeader(f)
	require.NoError(t, err)
	assert.Equal(t, want.Magic, got.Magic)
	assert.Equal(t, want.Version, got.Version)
	assert.Equal(t, want.ActiveBuffer, got.ActiveBuffer)
	assert.Equal(t, want.Sequence, got.Sequence)
}

func TestArenaHeader_CalculateActiveOffset_HappyPath(t *testing.T) {
	// File layout: [4KB header][4KB buffer0][4KB buffer1] = 12KB.
	// Active buffer 0 → offset 4096. Active buffer 1 → offset 8192.
	const fileSize = ArenaHeaderSize + 4096 + 4096

	tests := []struct {
		name   string
		active uint8
		want   int64
	}{
		{"buffer 0", 0, ArenaHeaderSize},
		{"buffer 1", 1, ArenaHeaderSize + 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &ArenaHeader{
				Magic: ArenaMagic, Version: 1, ActiveBuffer: tc.active,
			}
			got, err := h.CalculateActiveOffset(fileSize)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestArenaHeader_CalculateActiveOffset_RejectsInvalidMagic(t *testing.T) {
	h := &ArenaHeader{Magic: 0xDEADBEEF, Version: 1}
	_, err := h.CalculateActiveOffset(ArenaHeaderSize + 8192)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arena magic")
}

func TestArenaHeader_CalculateActiveOffset_RejectsUnsupportedVersion(t *testing.T) {
	h := &ArenaHeader{Magic: ArenaMagic, Version: 99}
	_, err := h.CalculateActiveOffset(ArenaHeaderSize + 8192)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported arena version")
}

func TestArenaHeader_CalculateActiveOffset_RejectsInvalidActiveBuffer(t *testing.T) {
	// Only 0 and 1 are valid (double-buffered). Anything else is corruption.
	h := &ArenaHeader{Magic: ArenaMagic, Version: 1, ActiveBuffer: 2}
	_, err := h.CalculateActiveOffset(ArenaHeaderSize + 8192)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid active buffer index")
}

func TestArenaHeader_CalculateActiveOffset_RejectsTooSmallFile(t *testing.T) {
	// File is only the header — no buffer space. Treating bufferSize=0
	// as "valid offset 4096" would let callers read past EOF.
	h := &ArenaHeader{Magic: ArenaMagic, Version: 1}
	_, err := h.CalculateActiveOffset(ArenaHeaderSize)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arena size")
}
