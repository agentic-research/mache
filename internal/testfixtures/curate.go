// Snapshot curation filter for the real-corpus fixture registry.
// See ADR-0019 section D.7 for the inclusion/exclusion spec.
//
// The filter takes a source directory + language and produces a curated
// copy at a destination directory. Only source files (per lang.Registry
// extensions for the requested language) and a small set of
// project-marker / documentation files are kept; build outputs, SCM
// metadata, and grammar-bomb binaries are excluded.
//
// Used by tools/fixtures-snapshot/ to bake external snapshots into
// testdata/snapshots/<id>/; the filter is also covered by package
// tests so the spec can't silently rot.
package testfixtures

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/lang"
)

// CurateOptions controls the snapshot filter.
type CurateOptions struct {
	// Source is the absolute path to the upstream working tree.
	Source string
	// Dest is the absolute path where the curated tree is written.
	// If Dest exists, it is removed first; callers should ensure they
	// pass a path they own.
	Dest string
	// Language is the canonical name from lang.Registry (e.g. "go",
	// "rust"). Only files with extensions matching this language's
	// Extensions slice are copied as "source" files; project-marker and
	// documentation files are copied via a per-language allowlist.
	Language string
}

// CurateResult reports what the filter produced.
type CurateResult struct {
	FilesCopied int
	BytesCopied int64
}

// Curate walks Source and writes a filtered copy under Dest per ADR-0019 D.7.
//
// Inclusion (any of):
//   - File extension matches lang.Registry[Language].Extensions
//   - File name is in the per-language project-marker allowlist (e.g.
//     Cargo.toml + Cargo.lock for Rust, go.mod + go.sum for Go)
//   - File is documentation (README.md / CHANGELOG.md at any level,
//     or *.md anywhere under a docs/ subtree)
//
// Exclusion (overrides inclusion):
//   - Any path component matches the build-output / SCM / IDE deny set
//     (target, node_modules, .git, .idea, .vscode, etc.)
//   - testdata/**/*.db and testdata/**/*.tar
//   - Files named parser.c anywhere (tree-sitter grammar bombs that
//     are valid C source but not the kind of source we want in fixtures)
//   - .DS_Store
//
// Returns the number of files and bytes copied. Errors from individual
// file copies are wrapped and returned immediately — partial state is
// left on disk for the caller to clean up.
func Curate(opts CurateOptions) (CurateResult, error) {
	if opts.Source == "" || opts.Dest == "" || opts.Language == "" {
		return CurateResult{}, fmt.Errorf("curate: Source, Dest, and Language are all required")
	}
	l := lang.ForName(opts.Language)
	if l == nil {
		return CurateResult{}, fmt.Errorf("curate: unknown language %q (not in lang.Registry)", opts.Language)
	}

	// Build a set of the language's source extensions for O(1) lookup.
	sourceExts := make(map[string]bool, len(l.Extensions))
	for _, ext := range l.Extensions {
		sourceExts[ext] = true
	}
	markers := projectMarkers(opts.Language)

	// Wipe and re-create the destination so re-snapshotting a fixture
	// produces a deterministic tree (no leftover files from a previous
	// curation that no longer match the filter).
	if err := os.RemoveAll(opts.Dest); err != nil {
		return CurateResult{}, fmt.Errorf("curate: clean dest %q: %w", opts.Dest, err)
	}
	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return CurateResult{}, fmt.Errorf("curate: mkdir dest %q: %w", opts.Dest, err)
	}

	var result CurateResult
	srcRoot := filepath.Clean(opts.Source)

	walkErr := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission-denied on individual files is non-fatal —
			// skip them so a single unreadable file in the source
			// tree doesn't abort the whole snapshot.
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		// Skip the root itself; nothing to copy and Rel returns ".".
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if isExcludedDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !shouldInclude(rel, d.Name(), sourceExts, markers) {
			return nil
		}
		dst := filepath.Join(opts.Dest, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("curate: mkdir %q: %w", filepath.Dir(dst), err)
		}
		n, err := copyFile(path, dst)
		if err != nil {
			return fmt.Errorf("curate: copy %q → %q: %w", path, dst, err)
		}
		result.FilesCopied++
		result.BytesCopied += n
		return nil
	})
	if walkErr != nil {
		return result, walkErr
	}
	return result, nil
}

// projectMarkers returns the per-language allowlist of non-source files
// that should be copied (project manifests, lockfiles, top-level docs).
// Documentation files (README.md, CHANGELOG.md, docs/*.md) are handled
// separately in shouldInclude so they apply across all languages.
func projectMarkers(language string) map[string]bool {
	switch language {
	case "rust":
		return map[string]bool{
			"Cargo.toml": true,
			"Cargo.lock": true,
		}
	case "go":
		return map[string]bool{
			"go.mod": true,
			"go.sum": true,
		}
	case "python":
		return map[string]bool{
			"pyproject.toml":   true,
			"setup.py":         true,
			"requirements.txt": true,
		}
	case "javascript", "typescript":
		return map[string]bool{
			"package.json":      true,
			"package-lock.json": true,
			"tsconfig.json":     true,
		}
	case "elixir":
		return map[string]bool{
			"mix.exs":  true,
			"mix.lock": true,
		}
	default:
		// Languages without a defined marker set still get source +
		// docs; the snapshot just won't include a manifest.
		return map[string]bool{}
	}
}

// excludedDirNames is the set of directory base-names that are pruned
// from the walk wherever they appear (top-level or nested). These are
// build outputs, SCM metadata, and IDE config — never source.
var excludedDirNames = map[string]bool{
	"target":       true, // Rust / Java
	"node_modules": true, // JS / TS
	"__pycache__":  true, // Python
	".git":         true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"bin":          true,
}

// isExcludedDir reports whether a directory should be pruned. Uses the
// relative path for path-specific rules (currently none) and the base
// name for the global deny list. The relative path is plumbed through
// so future rules like "exclude vendor/ only at top level" stay easy.
func isExcludedDir(rel, name string) bool {
	_ = rel
	return excludedDirNames[name]
}

// shouldInclude decides whether a file is kept in the snapshot.
// Source extensions, project markers, and documentation files are
// included; build artifacts (.pyc, .o), test-data binaries, and
// grammar bombs (parser.c) are excluded even if they fell through.
func shouldInclude(rel, name string, sourceExts, markers map[string]bool) bool {
	// Hard excludes that override everything else.
	if name == ".DS_Store" || name == "parser.c" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".pyc" || ext == ".o" {
		return false
	}
	// testdata/**/*.db and testdata/**/*.tar — these are pre-built
	// fixtures inside the source tree we're snapshotting; including
	// them creates snapshot-of-snapshot recursion and bloats the
	// output without adding source intelligence.
	if strings.HasPrefix(rel, "testdata"+string(filepath.Separator)) {
		if ext == ".db" || ext == ".tar" {
			return false
		}
	}

	// Inclusion paths.
	if sourceExts[ext] {
		return true
	}
	if markers[name] {
		return true
	}
	// Top-level README / CHANGELOG and anything under docs/ that is
	// markdown. The doc-drift rules treat markdown as a source
	// language so these files exercise real test paths.
	if name == "README.md" || name == "CHANGELOG.md" {
		return true
	}
	if ext == ".md" {
		// Allow markdown under any docs/ subtree (e.g. docs/, crate/docs/).
		parts := strings.Split(rel, string(filepath.Separator))
		for _, p := range parts[:len(parts)-1] {
			if p == "docs" {
				return true
			}
		}
	}
	return false
}

// copyFile copies src → dst preserving the mode bits, returning the
// number of bytes written. Wraps the open/copy/close dance so the
// walk callback stays readable. Errors from the deferred closes on the
// happy path are intentionally ignored — io.Copy already returned the
// bytes, and a close-on-read-side failure isn't load-bearing for
// snapshot integrity. The dst close error IS load-bearing (it surfaces
// out-of-space and short-write conditions) so we close it explicitly.
func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}
