package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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
			base := filepath.Base(path)
			if path != srcDir && (strings.HasPrefix(base, ".") ||
				base == "node_modules" || base == "target" ||
				base == "dist" || base == "build" || base == "__pycache__") {
				return filepath.SkipDir
			}
			return os.MkdirAll(dst, info.Mode())
		}

		// Skip non-regular files (symlinks, devices, etc.)
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
