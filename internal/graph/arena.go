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
	// ArenaVersion is the current wire-format. v1 arenas inferred payload
	// length from the SQLite header (db page_count × page_size); v2
	// records DataSize explicitly so readers stay format-agnostic.
	ArenaVersion = 2
	// arenaHeaderBytes is the on-disk size of the ArenaHeader struct.
	arenaHeaderBytes = 24
)

// ArenaHeader mirrors `rs/ll-core/core/src/layout.rs::ArenaHeader` in LLO.
// Byte layout: magic u32 | version u8 | active_buffer u8 | padding [2]u8 |
// sequence u64 | data_size u64 — total 24 bytes, aligned to a 4096-byte page.
type ArenaHeader struct {
	Magic        uint32
	Version      uint8
	ActiveBuffer uint8
	Padding      [2]byte
	Sequence     uint64
	// DataSize is the exact byte length of the live payload in the
	// active buffer. Lets a reader hash `buf[..DataSize]` and compare
	// against the controller's current_root without parsing the
	// payload's own format (e.g. SQLite page count).
	DataSize uint64
}

// ReadArenaHeader reads the on-disk header from the file.
func ReadArenaHeader(f *os.File) (*ArenaHeader, error) {
	buf := make([]byte, arenaHeaderBytes)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return nil, err
	}

	h := &ArenaHeader{
		Magic:        binary.LittleEndian.Uint32(buf[0:4]),
		Version:      buf[4],
		ActiveBuffer: buf[5],
		Sequence:     binary.LittleEndian.Uint64(buf[8:16]),
		DataSize:     binary.LittleEndian.Uint64(buf[16:24]),
	}

	return h, nil
}

// CalculateArenaOffset returns the byte offset of the active buffer.
func (h *ArenaHeader) CalculateActiveOffset(fileSize int64) (int64, error) {
	if h.Magic != ArenaMagic {
		return 0, fmt.Errorf("invalid arena magic: %x", h.Magic)
	}
	if h.Version != ArenaVersion {
		return 0, fmt.Errorf("unsupported arena version: file is v%d, expected v%d — ensure ley-line-open ≥ 0.2.0", h.Version, ArenaVersion)
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

// WriteArenaHeader serializes an ArenaHeader to the first 24 bytes at offset 0.
func WriteArenaHeader(f *os.File, h *ArenaHeader) error {
	buf := make([]byte, arenaHeaderBytes)
	binary.LittleEndian.PutUint32(buf[0:4], h.Magic)
	buf[4] = h.Version
	buf[5] = h.ActiveBuffer
	// buf[6:8] padding = 0
	binary.LittleEndian.PutUint64(buf[8:16], h.Sequence)
	binary.LittleEndian.PutUint64(buf[16:24], h.DataSize)
	_, err := f.WriteAt(buf, 0)
	return err
}

// CreateArena creates a fresh double-buffered arena file from a .db file.
// Layout: [Header (4KB)] [Buffer0 = dbBytes] [Buffer1 = zeros]
// Buffer size is the .db file size rounded up to the next 4KB page boundary.
func CreateArena(dbPath, arenaPath string) error {
	dbBytes, err := os.ReadFile(dbPath)
	if err != nil {
		return fmt.Errorf("read db: %w", err)
	}

	// Round buffer size up to 4KB page boundary
	bufferSize := int64(len(dbBytes))
	if bufferSize%ArenaHeaderSize != 0 {
		bufferSize = (bufferSize/ArenaHeaderSize + 1) * ArenaHeaderSize
	}

	f, err := os.Create(arenaPath)
	if err != nil {
		return fmt.Errorf("create arena: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Pre-allocate: header + 2 buffers
	totalSize := int64(ArenaHeaderSize) + 2*bufferSize
	if err := f.Truncate(totalSize); err != nil {
		return fmt.Errorf("truncate arena: %w", err)
	}

	// Write header
	h := &ArenaHeader{
		Magic:        ArenaMagic,
		Version:      ArenaVersion,
		ActiveBuffer: 0,
		Sequence:     1,
		DataSize:     uint64(len(dbBytes)),
	}
	if err := WriteArenaHeader(f, h); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	// Write DB bytes into Buffer0
	if _, err := f.WriteAt(dbBytes, ArenaHeaderSize); err != nil {
		return fmt.Errorf("write buffer0: %w", err)
	}

	return f.Sync()
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
