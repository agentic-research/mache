package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/internal/ingest"
)

// shouldSkipDir delegates to ingest.ShouldSkipDir.
func shouldSkipDir(base string) bool {
	return ingest.ShouldSkipDir(base)
}

// copyFile copies src to dst, creating dst if it doesn't exist.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyDir recursively copies srcDir to dstDir, skipping hidden dirs and
// common build artifact directories. Returns the number of files copied.
func copyDir(srcDir, dstDir string) (int, error) {
	copied := 0
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstDir, rel)

		if info.IsDir() {
			if path != srcDir && shouldSkipDir(filepath.Base(path)) {
				return filepath.SkipDir
			}
			return os.MkdirAll(dst, info.Mode())
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		if err := copyFile(path, dst); err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		copied++
		return nil
	})
	return copied, err
}

// dirSize walks dir and returns the total size of regular files in bytes,
// skipping the same hidden/build directories as copyDir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != dir && shouldSkipDir(filepath.Base(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
