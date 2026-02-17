package graph

import (
	"path/filepath"
	"strings"
)

// Virtual directory path helpers shared by FUSE (internal/fs) and NFS (internal/nfsmount).
// These parse callers/ and callees/ virtual directory paths without any Graph dependency.

// IsCallersPath returns true if the path contains a /callers segment boundary.
func IsCallersPath(path string) bool {
	return strings.HasSuffix(path, "/callers") || strings.Contains(path, "/callers/")
}

// ParseCallersPath splits a callers path into (parentDir, entryName).
// E.g. "/funcs/Foo/callers/funcs_Bar_source" → ("/funcs/Foo", "funcs_Bar_source")
// Returns ("", "") if not a valid callers path.
func ParseCallersPath(path string) (parentDir, entryName string) {
	return parseVDirPath(path, "/callers")
}

// IsCalleesPath returns true if the path contains a /callees segment boundary.
func IsCalleesPath(path string) bool {
	return strings.HasSuffix(path, "/callees") || strings.Contains(path, "/callees/")
}

// ParseCalleesPath splits a callees path into (parentDir, entryName).
// E.g. "/funcs/Foo/callees/funcs_Bar_source" → ("/funcs/Foo", "funcs_Bar_source")
func ParseCalleesPath(path string) (parentDir, entryName string) {
	return parseVDirPath(path, "/callees")
}

// VDirSymlinkTarget computes the relative symlink target from a virtual dir entry
// back to the target node in the graph. Works for both callers/ and callees/.
func VDirSymlinkTarget(vdirParentDir, targetID string) string {
	depth := strings.Count(vdirParentDir, "/") + 1 // +1 for the virtual dir itself
	return strings.Repeat("../", depth) + targetID
}

// FindSourceChild finds the "source" file child of a directory node.
// Returns the full source ID or "" if not found. Used by callees/ to
// resolve a callee directory to its source content.
func FindSourceChild(g Graph, dirID string) string {
	children, err := g.ListChildren(dirID)
	if err != nil {
		return ""
	}
	for _, child := range children {
		if filepath.Base(child) == "source" {
			if !strings.Contains(child, "/") {
				return dirID + "/" + child
			}
			return child
		}
	}
	return ""
}

// parseVDirPath is the generic implementation for parsing virtual directory paths.
func parseVDirPath(path, segment string) (parentDir, entryName string) {
	withSlash := segment + "/"
	idx := strings.Index(path, withSlash)
	if idx < 0 {
		if strings.HasSuffix(path, segment) {
			idx = len(path) - len(segment)
		} else {
			return "", ""
		}
	}
	parentDir = path[:idx]
	if parentDir == "" {
		parentDir = "/"
	}
	rest := path[idx+len(segment):]
	if rest == "" || rest == "/" {
		return parentDir, ""
	}
	entryName = strings.TrimPrefix(rest, "/")
	return parentDir, entryName
}
