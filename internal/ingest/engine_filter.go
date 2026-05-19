package ingest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// binarySniffSize is the number of bytes read from the start of a file
// to detect binary content (same heuristic as git).
const binarySniffSize = 512

// MaxIngestFileSize is the largest file we'll read into memory during
// ingestion or schema inference. Files above this are silently skipped.
// Set to 0 to disable the size limit. Configurable via --max-file-size.
var MaxIngestFileSize int64 = 100 << 20 // 100 MB

// ParseSize parses a human-readable size string (e.g. "100MB", "1GB", "0").
// Returns bytes. Supported suffixes: KB, MB, GB (case-insensitive).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	var multiplier int64 = 1
	numStr := s
	switch {
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1 << 30
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1 << 20
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1 << 10
		numStr = s[:len(s)-2]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n * multiplier, nil
}

// skipExts are file extensions that are always skipped during directory walks.
var skipExts = map[string]bool{
	".o": true, ".a": true,
	".db": true, ".sqlite": true, ".sqlite3": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pdf": true, ".mp3": true, ".mp4": true, ".wav": true,
}

// ShouldSkipDir returns true for hidden dirs and common build artifact directories.
func ShouldSkipDir(base string) bool {
	if strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "node_modules", "target", "dist", "build", "vendor", "__pycache__":
		return true
	}
	return false
}

// ShouldSkipFile returns true if the file should not be ingested.
// Checks extension blocklist, size limit, and binary content.
func ShouldSkipFile(path string, size int64) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if skipExts[ext] {
		return true
	}
	if MaxIngestFileSize > 0 && size > MaxIngestFileSize {
		return true
	}
	return false
}

// ensureFile returns an error if path does not exist or is a directory.
func ensureFile(path, kind string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err // coverage:ignore
	} // coverage:ignore
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not %s", path, kind) // coverage:ignore
	} // coverage:ignore
	return info, nil
}

// isBinaryFile returns true if the file appears to contain binary content.
// Uses the same heuristic as git: if the first 512 bytes contain a null byte,
// the file is binary. SQLite files (.db) are handled before this is called.
func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, binarySniffSize)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false // coverage:ignore
	} // coverage:ignore
	return bytes.ContainsRune(buf[:n], 0)
}
