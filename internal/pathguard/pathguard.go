// Package pathguard provides symlink-aware containment checks for paths that
// originate in configuration or API input.
package pathguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RequireContained verifies that path resolves within or equal to base.
// Nonexistent tails are reattached to the nearest symlink-resolved ancestor.
func RequireContained(path, base string) error {
	resolvedPath, err := resolveExistingAncestor(path)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", path, err)
	}
	resolvedBase, err := resolveExistingAncestor(base)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", base, err)
	}
	relative, err := filepath.Rel(resolvedBase, resolvedPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes project directory %q", path, base)
	}
	return nil
}

func resolveExistingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}

	dir := abs
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		tail = append(tail, filepath.Base(dir))
		dir = parent
		if _, err := os.Stat(dir); err == nil {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
	}
}
