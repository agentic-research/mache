// Package testfixtures is the real-corpus fixture registry (ADR-0019).
//
// Tests pull fixtures by ID via Get(t, id), which materializes a
// SQLiteGraph and caches it per-process for re-use across tests in the
// same package. Cleanup is automatic via t.Cleanup.
//
// PR 1 of ADR-0019 ships only the "mache-self" sentinel fixture which
// resolves to mache's own repo root at runtime via runtime.Caller.
// External snapshots (rosary, ley-line-open) are PR 2; baseline tracking
// is PR 3.
//
// See docs/adr/0019-real-corpus-fixture-registry.md.
package testfixtures

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	machetmpl "github.com/agentic-research/mache/internal/template"
)

// Fixture is one manifest entry. See docs/adr/0019-real-corpus-fixture-registry.md
// section D.4 for the manifest format.
type Fixture struct {
	ID           string `toml:"id"`
	Tier         string `toml:"tier"`          // "medium" | "large"
	Language     string `toml:"language"`      // "go" | "rust" | "polyglot"
	Source       string `toml:"source"`        // "self" for sentinel, else URL or repo path
	SHA          string `toml:"sha"`           // pinned (empty for sentinel)
	Path         string `toml:"path"`          // relative to testdata/snapshots/, OR "$REPO_ROOT" for sentinel
	SchemaPreset string `toml:"schema_preset"` // "go" | "rust" | etc.
	License      string `toml:"license"`       // "own" | "MIT" | etc.
}

// manifestDoc is the on-disk shape of testdata/snapshots/manifest.toml.
type manifestDoc struct {
	Schema   string    `toml:"schema"`
	Fixtures []Fixture `toml:"fixture"`
}

// repoRoot is mache's own repo root, found once via runtime.Caller.
// Used both to resolve the manifest file and to expand the "$REPO_ROOT"
// sentinel path for fixtures with source = "self".
var (
	repoRootOnce sync.Once
	repoRootVal  string
	repoRootErr  error
)

// findRepoRoot walks up from this file's directory until it finds a
// go.mod. Mirrors cmd/all_tools_self_test.go::macheRepoRoot but
// resolved at package init time (test-binary-relative).
func findRepoRoot() (string, error) {
	repoRootOnce.Do(func() {
		_, here, _, ok := runtime.Caller(0)
		if !ok {
			repoRootErr = fmt.Errorf("runtime.Caller(0) failed; cannot locate mache repo root")
			return
		}
		dir := filepath.Dir(here)
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				repoRootVal = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				repoRootErr = fmt.Errorf("walked to filesystem root without finding go.mod (started at %s)", here)
				return
			}
			dir = parent
		}
	})
	return repoRootVal, repoRootErr
}

// manifest is the parsed manifest.toml, loaded once on first access.
var (
	manifestOnce sync.Once
	manifestVal  []Fixture
	manifestByID map[string]Fixture
	manifestErr  error
)

func loadManifest() error {
	manifestOnce.Do(func() {
		root, err := findRepoRoot()
		if err != nil {
			manifestErr = err
			return
		}
		path := filepath.Join(root, "testdata", "snapshots", "manifest.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			manifestErr = fmt.Errorf("read manifest %s: %w", path, err)
			return
		}
		var doc manifestDoc
		if err := toml.Unmarshal(data, &doc); err != nil {
			manifestErr = fmt.Errorf("parse manifest %s: %w", path, err)
			return
		}
		if doc.Schema != "mache-fixtures/v1" {
			manifestErr = fmt.Errorf("unsupported manifest schema %q (want mache-fixtures/v1)", doc.Schema)
			return
		}
		manifestVal = doc.Fixtures
		manifestByID = make(map[string]Fixture, len(doc.Fixtures))
		for _, f := range doc.Fixtures {
			if _, dup := manifestByID[f.ID]; dup {
				manifestErr = fmt.Errorf("duplicate fixture id %q in manifest", f.ID)
				return
			}
			manifestByID[f.ID] = f
		}
	})
	return manifestErr
}

// All returns the loaded manifest entries (for diagnostic tooling).
// Tests typically use Get; this is for `mache fixtures list` or similar.
func All() []Fixture {
	_ = loadManifest()
	out := make([]Fixture, len(manifestVal))
	copy(out, manifestVal)
	return out
}

// Lookup returns the Fixture entry for the given id, or false if unknown.
// Exposed for diagnostic tooling and tests that want to inspect manifest
// fields without materializing the graph.
func Lookup(id string) (Fixture, bool) {
	if err := loadManifest(); err != nil {
		return Fixture{}, false
	}
	f, ok := manifestByID[id]
	return f, ok
}

// ResolvePath returns the on-disk source-tree path for a fixture.
// "$REPO_ROOT" sentinels expand to mache's own repo root. Other paths
// resolve relative to testdata/snapshots/.
func ResolvePath(id string) (string, error) {
	if err := loadManifest(); err != nil {
		return "", err
	}
	f, ok := manifestByID[id]
	if !ok {
		return "", fmt.Errorf("unknown fixture id %q", id)
	}
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	if f.Source == "self" || f.Path == "$REPO_ROOT" {
		return root, nil
	}
	if filepath.IsAbs(f.Path) {
		return f.Path, nil
	}
	return filepath.Join(root, "testdata", "snapshots", f.Path), nil
}

// LoadSchema loads the preset schema for a fixture by parsing the
// embedded schema file from cmd/schemas/. Mirrors cmd.resolveSchema for
// the preset case (this package can't import cmd/ — cyclical).
func LoadSchema(id string) (*api.Topology, error) {
	if err := loadManifest(); err != nil {
		return nil, err
	}
	f, ok := manifestByID[id]
	if !ok {
		return nil, fmt.Errorf("unknown fixture id %q", id)
	}
	if f.SchemaPreset == "" {
		return nil, fmt.Errorf("fixture %q has no schema_preset", id)
	}
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	schemaPath := filepath.Join(root, "cmd", "schemas", f.SchemaPreset+".json")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("read preset schema %q: %w", f.SchemaPreset, err)
	}
	var topo api.Topology
	if err := json.Unmarshal(data, &topo); err != nil {
		return nil, fmt.Errorf("parse preset schema %q: %w", f.SchemaPreset, err)
	}
	return &topo, nil
}

// cachedGraph holds a materialized SQLiteGraph plus its tempdir.
//
// Cross-test reuse: the cache lives for the process lifetime of the
// test binary so subsequent Get(t, id) calls within `go test
// ./pkg/...` reuse the same SQLiteGraph + .db file. The graph is NOT
// torn down per-test — that would defeat the cache. Resources are
// released only when the test binary exits (OS reaps the tempdir;
// SQLite handles close on process exit).
//
// This means a misbehaving test could pollute the graph for later
// tests in the same binary. SQLiteGraph is effectively read-only via
// the public API today; if a future mutation path is added the cache
// strategy needs revisiting.
type cachedGraph struct {
	g       *graph.SQLiteGraph
	tempDir string
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*cachedGraph{}
)

// Get materializes a fixture and returns an open SQLiteGraph for the test.
// Caches per-process so repeated calls within one `go test` invocation
// reuse the same .db. Cleanup is automatic via t.Cleanup at test-binary exit.
//
// On unknown fixture id, calls t.Fatalf — the test fails loudly rather
// than returning nil.
func Get(t *testing.T, id string) *graph.SQLiteGraph {
	t.Helper()

	cacheMu.Lock()
	if c, ok := cache[id]; ok {
		cacheMu.Unlock()
		return c.g
	}
	cacheMu.Unlock()

	// Resolve manifest entry.
	if err := loadManifest(); err != nil {
		t.Fatalf("testfixtures.Get(%q): load manifest: %v", id, err)
	}
	if _, ok := manifestByID[id]; !ok {
		t.Fatalf("testfixtures.Get(%q): unknown fixture id (available: %v)", id, knownIDs())
	}

	srcPath, err := ResolvePath(id)
	if err != nil {
		t.Fatalf("testfixtures.Get(%q): resolve path: %v", id, err)
	}
	schema, err := LoadSchema(id)
	if err != nil {
		t.Fatalf("testfixtures.Get(%q): load schema: %v", id, err)
	}

	// Build the .db in a process-lifetime temp dir (not t.TempDir —
	// that gets cleaned up per-test, defeating the cache). We register
	// our own cleanup via t.Cleanup so the LAST test to run gets the
	// removal, but the directory survives across tests in the binary.
	tempDir, err := os.MkdirTemp("", "mache-fixture-"+sanitizeID(id)+"-*")
	if err != nil {
		t.Fatalf("testfixtures.Get(%q): mktempdir: %v", id, err)
	}
	dbPath := filepath.Join(tempDir, "fixture.db")

	writer, err := ingest.NewSQLiteWriter(dbPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("testfixtures.Get(%q): sqlite writer: %v", id, err)
	}
	engine := ingest.NewEngine(schema, writer)
	if err := engine.Ingest(srcPath); err != nil {
		_ = writer.Close()
		_ = os.RemoveAll(tempDir)
		t.Fatalf("testfixtures.Get(%q): ingest %s: %v", id, srcPath, err)
	}
	if err := writer.Close(); err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("testfixtures.Get(%q): writer close: %v", id, err)
	}
	sg, err := graph.OpenSQLiteGraph(dbPath, schema, machetmpl.Render)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("testfixtures.Get(%q): open sqlite graph: %v", id, err)
	}

	cacheMu.Lock()
	// Race: another test might have populated the cache while we were
	// ingesting. If so, discard ours and return theirs.
	if existing, ok := cache[id]; ok {
		cacheMu.Unlock()
		_ = sg.Close()
		_ = os.RemoveAll(tempDir)
		return existing.g
	}
	cache[id] = &cachedGraph{g: sg, tempDir: tempDir}
	cacheMu.Unlock()
	return sg
}

// RequireTier skips the test unless the tier's env-var gate is set.
//
// Tiers:
//   - "medium" — always on; no env var required.
//   - "large"  — requires MACHE_E2E_LARGE=1 (perf-gate / nightly only).
//
// Unknown tiers fatal-fail rather than skipping silently.
func RequireTier(t *testing.T, tier string) {
	t.Helper()
	switch tier {
	case "medium":
		return
	case "large":
		if os.Getenv("MACHE_E2E_LARGE") == "" {
			t.Skipf("large-tier fixture; set MACHE_E2E_LARGE=1 to enable")
		}
		return
	default:
		t.Fatalf("testfixtures.RequireTier: unknown tier %q (want medium|large)", tier)
	}
}

// knownIDs returns the sorted list of fixture IDs for error messages.
func knownIDs() []string {
	ids := make([]string, 0, len(manifestByID))
	for id := range manifestByID {
		ids = append(ids, id)
	}
	return ids
}

// sanitizeID makes a fixture id safe to use in a tempdir name.
// Replaces path separators and slashes; leaves alphanumerics + dash + underscore.
func sanitizeID(id string) string {
	buf := make([]byte, 0, len(id))
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			buf = append(buf, c)
		default:
			buf = append(buf, '_')
		}
	}
	return string(buf)
}
