package writeback

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/internal/graph"
)

// Splice replaces the byte range identified by origin with newContent in the source file.
// The write is atomic: content is written to a temp file first, then renamed.
func Splice(origin graph.SourceOrigin, newContent []byte) error {
	src, err := os.ReadFile(origin.FilePath)
	if err != nil {
		return fmt.Errorf("read source %s: %w", origin.FilePath, err)
	}

	start := origin.StartByte
	end := origin.EndByte

	if int(start) > len(src) || int(end) > len(src) || start > end {
		return fmt.Errorf("invalid byte range [%d:%d] for file of length %d", start, end, len(src))
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

	// Preserve original file permissions
	info, err := os.Stat(origin.FilePath)
	if err == nil {
		_ = os.Chmod(tmpName, info.Mode()) // best-effort permission sync
	}

	if err := os.Rename(tmpName, origin.FilePath); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("rename temp to %s: %w", origin.FilePath, err)
	}

	return nil
}
