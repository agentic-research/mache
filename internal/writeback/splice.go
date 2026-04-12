package writeback

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/internal/graph"
)

// MaxSpliceFileSize is the maximum source file size Splice will read.
// Prevents OOM on accidentally large files (e.g., generated code, vendored blobs).
const MaxSpliceFileSize = 100 * 1024 * 1024 // 100MB

// Splice replaces the byte range identified by origin with newContent in the source file.
// The write is atomic: content is written to a temp file first, then renamed.
func Splice(origin graph.SourceOrigin, newContent []byte) error {
	info, err := os.Stat(origin.FilePath)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", origin.FilePath, err)
	}
	if info.Size() > MaxSpliceFileSize {
		return fmt.Errorf("source file %s is %d bytes (max %d)", origin.FilePath, info.Size(), MaxSpliceFileSize)
	}

	src, err := os.ReadFile(origin.FilePath)
	if err != nil {
		return fmt.Errorf("read source %s: %w", origin.FilePath, err)
	}

	start := origin.StartByte
	end := origin.EndByte

	if int(start) > len(src) || int(end) > len(src) || start > end {
		return fmt.Errorf("invalid byte range [%d:%d] for file of length %d", start, end, len(src))
	}

	// Normalize trailing newlines: match the original region's pattern.
	// Agents often write via echo/heredoc which appends a trailing \n that
	// wasn't present in the original source region. Strip it to avoid
	// introducing blank-line artifacts.
	originalRegion := src[start:end]
	if len(originalRegion) > 0 && originalRegion[len(originalRegion)-1] != '\n' {
		newContent = bytes.TrimRight(newContent, "\n")
	}

	// result = prefix + newContent + suffix
	result := make([]byte, 0, int(start)+len(newContent)+len(src)-int(end))
	result = append(result, src[:start]...)
	result = append(result, newContent...)
	result = append(result, src[end:]...)

	// Atomic write: temp file in same dir, then rename
	dir := filepath.Dir(origin.FilePath)
	tmp, err := os.CreateTemp(dir, ".mache-splice-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(result); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("close temp: %w", err)
	}

	// Preserve original file permissions (reuse stat from size guard)
	_ = os.Chmod(tmpName, info.Mode())

	if err := os.Rename(tmpName, origin.FilePath); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("rename temp to %s: %w", origin.FilePath, err)
	}

	return nil
}
