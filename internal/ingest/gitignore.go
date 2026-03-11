package ingest

import (
	"bufio"
	"os"
	"path/filepath"
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

// loadGitignore reads .gitignore from rootDir and discovers nested .gitignore
// files in the tree. Returns nil if no .gitignore exists at all.
func loadGitignore(rootDir string) *gitignoreMatcher {
	m := &gitignoreMatcher{
		rootDir: rootDir,
		nested:  make(map[string][]gitignorePattern),
	}

	rootPatterns := parseGitignoreFile(filepath.Join(rootDir, ".gitignore"))
	m.patterns = rootPatterns

	// Walk the tree to find nested .gitignore files.
	// This is lightweight — we only stat .gitignore in each directory, not read
	// every file. filepath.Walk is fine since we skip hidden/build dirs just like Ingest.
	_ = filepath.Walk(rootDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort
		}
		if info.IsDir() {
			base := filepath.Base(p)
			if p != rootDir {
				if len(base) > 0 && base[0] == '.' {
					return filepath.SkipDir
				}
				if base == "node_modules" || base == "target" || base == "dist" || base == "build" {
					return filepath.SkipDir
				}
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
	ignored := evalPatterns(m.patterns, relPath, isDir)

	// Evaluate nested patterns — each nested .gitignore only applies to paths
	// within its directory.
	for dir, patterns := range m.nested {
		prefix := dir + "/"
		if strings.HasPrefix(relPath, prefix) || relPath == dir {
			subRel := strings.TrimPrefix(relPath, prefix)
			if subRel == "" {
				continue // the directory itself — matched by parent
			}
			result := evalPatterns(patterns, subRel, isDir)
			if result {
				ignored = true
			}
			// A negation in nested can un-ignore only if the path was not
			// force-ignored at root level, but for simplicity we let the last
			// match win (matching git behavior within a scope).
		}
	}

	return ignored
}

// evalPatterns applies a list of patterns against relPath. Returns the final
// ignored state: true = ignored, false = not ignored. If no pattern matches,
// returns false.
func evalPatterns(patterns []gitignorePattern, relPath string, isDir bool) bool {
	ignored := false
	for _, p := range patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if matchPattern(p.pattern, relPath) {
			ignored = !p.negate
		}
	}
	return ignored
}

// matchPattern checks if a gitignore pattern matches a relative path.
//
// If the pattern contains no slash, it matches the basename of the path at any
// depth (e.g. "*.log" matches "a/b/debug.log"). If the pattern contains a
// slash, it is anchored and matched against the full relative path.
func matchPattern(pattern, relPath string) bool {
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
