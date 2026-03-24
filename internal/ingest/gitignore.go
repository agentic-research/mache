package ingest

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gitignorePattern represents a single parsed line from a .gitignore file.
type gitignorePattern struct {
	pattern string // the glob pattern (after stripping negation/trailing slash)
	negate  bool   // true if the line started with '!'
	dirOnly bool   // true if the line ended with '/'
}

// gitignoreMatcher holds a hierarchy of gitignore rules: root-level patterns
// plus per-directory overrides from nested .gitignore files.
type gitignoreMatcher struct {
	rootDir  string
	patterns []gitignorePattern            // patterns from the root .gitignore
	nested   map[string][]gitignorePattern // dir (relative to rootDir) → patterns
}

// LoadGitignore reads .gitignore from rootDir and discovers nested .gitignore
// files in the tree. Returns nil if no .gitignore exists at all.
func LoadGitignore(rootDir string) *gitignoreMatcher {
	m := &gitignoreMatcher{
		rootDir: rootDir,
		nested:  make(map[string][]gitignorePattern),
	}

	rootPatterns := parseGitignoreFile(filepath.Join(rootDir, ".gitignore"))
	m.patterns = rootPatterns

	// Walk the tree to find nested .gitignore files.
	// This is lightweight — we only stat .gitignore in each directory, not read
	// every file. fs.WalkDir avoids extra Lstat calls since DirEntry carries the type.
	_ = filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort
		}
		if d.IsDir() {
			if p != rootDir && ShouldSkipDir(filepath.Base(p)) {
				return filepath.SkipDir
			}
			// Check for nested .gitignore (skip root, already loaded)
			if p != rootDir {
				nestedPath := filepath.Join(p, ".gitignore")
				if patterns := parseGitignoreFile(nestedPath); len(patterns) > 0 {
					rel, _ := filepath.Rel(rootDir, p)
					rel = filepath.ToSlash(rel)
					m.nested[rel] = patterns
				}
			}
			return nil
		}
		return nil
	})

	return m
}

// parseGitignoreFile reads and parses a single .gitignore file.
// Returns nil if the file does not exist or cannot be read.
func parseGitignoreFile(path string) []gitignorePattern {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var patterns []gitignorePattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip blank lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		p := gitignorePattern{}

		// Negation
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = line[1:]
		}

		// Directory-only marker
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}

		p.pattern = line
		if p.pattern != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

// Match reports whether a path (relative to rootDir, forward-slash separated)
// should be ignored. isDir must be true when the path refers to a directory.
//
// Matching follows gitignore semantics:
//   - Patterns without a slash match the basename at any level.
//   - Patterns with a slash are anchored to the .gitignore's directory.
//   - Later patterns override earlier ones; negation patterns un-ignore.
//   - dirOnly patterns only match directories.
//   - Nested .gitignore patterns are scoped to their directory.
func (m *gitignoreMatcher) Match(relPath string, isDir bool) bool {
	if m == nil {
		return false
	}

	// Evaluate root patterns
	ignored, _ := evalPatterns(m.patterns, relPath, isDir)

	// Sort nested dirs by depth (shallowest first) for deterministic evaluation.
	// Git evaluates from shallowest to deepest — closer .gitignore wins over parent.
	dirs := make([]string, 0, len(m.nested))
	for dir := range m.nested {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") < strings.Count(dirs[j], "/")
	})

	// Evaluate nested patterns — each nested .gitignore only applies to paths
	// within its directory.
	for _, dir := range dirs {
		patterns := m.nested[dir]
		prefix := dir + "/"
		if strings.HasPrefix(relPath, prefix) || relPath == dir {
			subRel := strings.TrimPrefix(relPath, prefix)
			if subRel == "" {
				continue // the directory itself — matched by parent
			}
			result, matched := evalPatterns(patterns, subRel, isDir)
			if matched {
				ignored = result
			}
		}
	}

	return ignored
}

// evalPatterns applies a list of patterns against relPath. Returns (ignored, matched).
// ignored is the final ignored state (true = ignored, false = not ignored).
// matched is true if any pattern in the list matched the path.
func evalPatterns(patterns []gitignorePattern, relPath string, isDir bool) (bool, bool) {
	ignored := false
	matched := false
	for _, p := range patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if matchPattern(p.pattern, relPath) {
			ignored = !p.negate
			matched = true
		}
	}
	return ignored, matched
}

// matchPattern checks if a gitignore pattern matches a relative path.
//
// If the pattern contains no slash, it matches the basename of the path at any
// depth (e.g. "*.log" matches "a/b/debug.log"). If the pattern contains a
// slash, it is anchored and matched against the full relative path.
func matchPattern(pattern, relPath string) bool {
	// Handle ** (double-star) patterns — filepath.Match doesn't support these
	if strings.Contains(pattern, "**") {
		return matchDoublestar(pattern, relPath)
	}

	if strings.Contains(pattern, "/") {
		// Anchored pattern — match against full path
		ok, _ := filepath.Match(pattern, relPath)
		return ok
	}

	// Unanchored — match against basename at every level.
	// First try the basename (most common case).
	base := filepath.Base(relPath)
	if ok, _ := filepath.Match(pattern, base); ok {
		return true
	}

	// Also match each path component for directory patterns.
	parts := strings.Split(relPath, "/")
	for _, part := range parts {
		if ok, _ := filepath.Match(pattern, part); ok {
			return true
		}
	}

	return false
}

// matchDoublestar handles gitignore ** patterns:
//
//	**/foo  — matches "foo" at any depth
//	foo/**  — matches anything inside "foo"
//	a/**/b  — matches "a/b", "a/x/b", "a/x/y/b", etc.
func matchDoublestar(pattern, relPath string) bool {
	// "**/rest" — match "rest" at any depth
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		// Try matching against the full path and each sub-path
		if matchPattern(suffix, relPath) {
			return true
		}
		parts := strings.Split(relPath, "/")
		for i := 1; i < len(parts); i++ {
			sub := strings.Join(parts[i:], "/")
			if matchPattern(suffix, sub) {
				return true
			}
		}
		return false
	}

	// "prefix/**" — match anything inside prefix
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3]
		return strings.HasPrefix(relPath, prefix+"/") || relPath == prefix
	}

	// "a/**/b" — match with any number of intermediate directories
	parts := strings.SplitN(pattern, "/**/", 2)
	if len(parts) == 2 {
		prefix, suffix := parts[0], parts[1]
		if !strings.HasPrefix(relPath, prefix+"/") {
			return false
		}
		rest := relPath[len(prefix)+1:]
		// Try matching suffix at each depth
		relParts := strings.Split(rest, "/")
		for i := 0; i < len(relParts); i++ {
			sub := strings.Join(relParts[i:], "/")
			if matchPattern(suffix, sub) {
				return true
			}
		}
		return false
	}

	return false
}
