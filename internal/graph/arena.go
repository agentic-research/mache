package graph

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	ArenaHeaderSize = 4096
	ArenaMagic      = 0x4C455930
)

type ArenaHeader struct {
	Magic        uint32
	Version      uint8
	ActiveBuffer uint8
	Padding      [2]byte
	Sequence     uint64
}

// ReadArenaHeader reads the 4KB header from the file.
func ReadArenaHeader(f *os.File) (*ArenaHeader, error) {
	buf := make([]byte, 16) // Struct size is 16 bytes
	if _, err := f.ReadAt(buf, 0); err != nil {
		return nil, err
	}

	h := &ArenaHeader{
		Magic:        binary.LittleEndian.Uint32(buf[0:4]),
		Version:      buf[4],
		ActiveBuffer: buf[5],
		Sequence:     binary.LittleEndian.Uint64(buf[8:16]),
	}

	return h, nil
}

// CalculateArenaOffset returns the byte offset of the active buffer.
func (h *ArenaHeader) CalculateActiveOffset(fileSize int64) (int64, error) {
	if h.Magic != ArenaMagic {
		return 0, fmt.Errorf("invalid arena magic: %x", h.Magic)
	}
	if h.Version != 1 {
		return 0, fmt.Errorf("unsupported arena version: %d", h.Version)
	}
	if h.ActiveBuffer > 1 {
		return 0, fmt.Errorf("invalid active buffer index: %d", h.ActiveBuffer)
	}

	bufferSize := (fileSize - ArenaHeaderSize) / 2
	if bufferSize <= 0 {
		return 0, fmt.Errorf("invalid arena size: %d", fileSize)
	}

	offset := int64(ArenaHeaderSize) + int64(h.ActiveBuffer)*bufferSize
	return offset, nil
}

// ExtractActiveDB extracts the active SQLite database from the arena to a temp file.
// Returns the path to the temp file.
func ExtractActiveDB(arenaPath string) (string, error) {
	f, err := os.Open(arenaPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	header, err := ReadArenaHeader(f)
	if err != nil {
		return "", fmt.Errorf("read header: %w", err)
	}

	offset, err := header.CalculateActiveOffset(info.Size())
	if err != nil {
		return "", err
	}

	// Calculate size (half of arena minus header)
	size := (info.Size() - ArenaHeaderSize) / 2

	// Create temp file — remove on any error after this point.
	tmp, err := os.CreateTemp("", "mache-arena-*.db")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	// Copy efficiently using io.CopyN
	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}

	if _, err := io.CopyN(tmp, f, size); err != nil {
		return "", fmt.Errorf("copy active db: %w", err)
	}

	cleanup = false
	return tmpPath, nil
}
