// fixtures-snapshot snapshots an external repo into
// testdata/snapshots/<id>/ using the curation filter from
// internal/testfixtures. Driven from `task fixtures:snapshot`.
//
// Usage:
//
//	go run ./tools/fixtures-snapshot --id medium-rust-rosary --source ~/remotes/art/rosary
//
// The fixture id MUST already exist in testdata/snapshots/manifest.toml;
// the tool looks up language + path from the manifest, applies the
// curation filter, writes the result, and prints a summary (file count,
// total bytes, source HEAD SHA).
//
// Source resolution:
//   - Local path (default): treated as the upstream working tree;
//     filter runs against it directly.
//   - Git URL (TODO; not in v1): would clone to a scratch dir at the
//     fixture's pinned SHA, run the filter, discard the clone.
//
// SHA detection: if Source is a local git checkout, the tool reads
// HEAD via `git rev-parse --short HEAD`. It prints the SHA so the
// caller can update manifest.toml — the tool itself does NOT rewrite
// manifest.toml because that file's hand-authored ordering is the
// source of truth for fixture metadata and an in-place TOML rewrite
// would normalize comments away.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/agentic-research/mache/internal/testfixtures"
)

type manifestDoc struct {
	Schema   string                 `toml:"schema"`
	Fixtures []testfixtures.Fixture `toml:"fixture"`
}

func main() {
	id := flag.String("id", "", "fixture id from testdata/snapshots/manifest.toml (required)")
	source := flag.String("source", "", "absolute path to upstream working tree (required)")
	flag.Parse()

	if *id == "" || *source == "" {
		fmt.Fprintln(os.Stderr, "usage: fixtures-snapshot --id <fixture-id> --source <path>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	if err := run(*id, *source); err != nil {
		fmt.Fprintf(os.Stderr, "fixtures-snapshot: %v\n", err)
		os.Exit(1)
	}
}

func run(id, source string) error {
	source = expandHome(source)
	absSource, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}
	if info, err := os.Stat(absSource); err != nil {
		return fmt.Errorf("source path %q: %w", absSource, err)
	} else if !info.IsDir() {
		return fmt.Errorf("source path %q is not a directory", absSource)
	}

	repoRoot, err := findMacheRoot()
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(repoRoot, "testdata", "snapshots", "manifest.toml")
	doc, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}

	fx, ok := lookup(doc, id)
	if !ok {
		known := make([]string, 0, len(doc.Fixtures))
		for _, f := range doc.Fixtures {
			known = append(known, f.ID)
		}
		return fmt.Errorf("fixture id %q not in manifest (known: %s)",
			id, strings.Join(known, ", "))
	}
	if fx.Source == "self" {
		return fmt.Errorf("fixture %q is the mache-self sentinel; it is not snapshot-able", id)
	}
	if fx.Language == "" {
		return fmt.Errorf("fixture %q has no language set in manifest", id)
	}
	if fx.Path == "" || strings.HasPrefix(fx.Path, "$") {
		return fmt.Errorf("fixture %q has no concrete snapshot path (got %q)", id, fx.Path)
	}

	destDir := filepath.Join(repoRoot, "testdata", "snapshots", fx.Path)
	fmt.Printf("[fixtures-snapshot] id=%s language=%s\n", id, fx.Language)
	fmt.Printf("[fixtures-snapshot] source=%s\n", absSource)
	fmt.Printf("[fixtures-snapshot] dest=%s\n", destDir)
	fmt.Println("[fixtures-snapshot] applying curation filter (per ADR-0019 D.7)...")

	res, err := testfixtures.Curate(testfixtures.CurateOptions{
		Source:   absSource,
		Dest:     destDir,
		Language: fx.Language,
	})
	if err != nil {
		return fmt.Errorf("curate: %w", err)
	}

	sha := detectGitSHA(absSource)

	fmt.Println("[fixtures-snapshot] done.")
	fmt.Printf("  files copied : %d\n", res.FilesCopied)
	fmt.Printf("  bytes copied : %d (%.2f MB)\n", res.BytesCopied, float64(res.BytesCopied)/(1024*1024))
	fmt.Printf("  source SHA   : %s\n", sha)
	fmt.Printf("  manifest     : %s\n", manifestPath)
	if sha != "" && sha != fx.SHA {
		fmt.Printf("  ACTION       : update manifest.toml fixture %q sha = %q (was %q)\n", id, sha, fx.SHA)
	}
	return nil
}

func loadManifest(path string) (*manifestDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	}
	var doc manifestDoc
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest %q: %w", path, err)
	}
	return &doc, nil
}

func lookup(doc *manifestDoc, id string) (testfixtures.Fixture, bool) {
	for _, f := range doc.Fixtures {
		if f.ID == id {
			return f, true
		}
	}
	return testfixtures.Fixture{}, false
}

// findMacheRoot walks up from this file's location until it finds a
// go.mod. The tool is built and run via `go run`, so runtime.Caller(0)
// points at this main.go regardless of cwd.
func findMacheRoot() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("walked to filesystem root without finding go.mod (started at %s)", here)
		}
		dir = parent
	}
}

// detectGitSHA returns the short HEAD SHA of source if it's a git
// checkout. Returns "" if not (or git unavailable) — caller decides
// whether that's fatal.
func detectGitSHA(source string) string {
	cmd := exec.Command("git", "-C", source, "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// expandHome resolves a leading "~/" to the user's home dir. Mirrors
// the helper in cmd/all_tools_self_test.go so the tool accepts the
// same path shapes a developer uses on the command line.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
