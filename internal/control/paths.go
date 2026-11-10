package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/machehome"
)

func validatePath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	if safeHome, err := machehome.Dir(); err == nil {
		if isUnder(abs, safeHome) {
			return nil
		}
	}

	if isUnder(abs, os.TempDir()) {
		return nil
	}

	return fmt.Errorf("security violation: control file path %q must be under ~/.mache or %s", abs, os.TempDir())
}

func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
