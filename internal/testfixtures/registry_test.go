package testfixtures

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistry_Get_MacheSelfReturnsLiveGraph proves the sentinel
// fixture resolves to mache's own repo root, builds an end-to-end
// SQLiteGraph, and the resulting graph has structure.
func TestRegistry_Get_MacheSelfReturnsLiveGraph(t *testing.T) {
	// mache-self projects the WHOLE mache repo. Since ADR-0012 step 4
	// (mache-37ae8b) removed in-process tree-sitter, this routes through
	// leyline parse + the ASTWalker schema projection, which is
	// pathologically slow at repo scale (mache-4f3840 — a v0.18.0 release
	// blocker). Gate behind the large-tier opt-in until that lands.
	if os.Getenv("MACHE_E2E_LARGE") == "" {
		t.Skip("whole-repo mache-self projection is slow (mache-4f3840); set MACHE_E2E_LARGE=1 to run")
	}

	g := Get(t, "mache-self")
	require.NotNil(t, g, "Get must return a non-nil SQLiteGraph")

	// Root should list at least one directory child. The exact
	// structure depends on the go preset schema's projection of
	// mache's source tree, but it MUST be non-empty — if it is the
	// ingest silently failed and the perf gate that uses this graph
	// would be exercising air.
	children, err := g.ListChildren("")
	require.NoError(t, err, "ListChildren must succeed on the projected root")
	require.NotEmpty(t, children, "root must have at least one child after ingest")
}

// TestRegistry_Get_UnknownIDFails asserts unknown fixture IDs surface
// as a hard test failure (t.Fatalf) rather than silently returning nil.
//
// We can't observe Fatalf on the parent *testing.T without itself
// failing — so we cover ResolvePath, which exercises the same lookup
// path and returns a plain error. That proves the manifest lookup
// rejects unknown ids; Get wraps the same lookup in a t.Fatalf.
func TestRegistry_Get_UnknownIDFails(t *testing.T) {
	_, err := ResolvePath("no-such-fixture-id")
	require.Error(t, err, "ResolvePath must reject unknown fixture ids")
	assert.Contains(t, err.Error(), "unknown fixture id",
		"error must name the failure mode for diagnostics")

	_, err = LoadSchema("no-such-fixture-id")
	require.Error(t, err, "LoadSchema must reject unknown fixture ids")

	_, ok := Lookup("no-such-fixture-id")
	assert.False(t, ok, "Lookup must return ok=false for unknown ids")
}

// TestRegistry_Get_CachesPerProcess proves repeated calls within the
// same test binary return the SAME *SQLiteGraph pointer — the
// fixture is materialized once and reused.
func TestRegistry_Get_CachesPerProcess(t *testing.T) {
	// mache-self projects the WHOLE mache repo. Since ADR-0012 step 4
	// (mache-37ae8b) removed in-process tree-sitter, this routes through
	// leyline parse + the ASTWalker schema projection, which is
	// pathologically slow at repo scale (mache-4f3840 — a v0.18.0 release
	// blocker). Gate behind the large-tier opt-in until that lands.
	if os.Getenv("MACHE_E2E_LARGE") == "" {
		t.Skip("whole-repo mache-self projection is slow (mache-4f3840); set MACHE_E2E_LARGE=1 to run")
	}

	first := Get(t, "mache-self")
	second := Get(t, "mache-self")
	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.Same(t, first, second, "repeated Get must return the cached pointer")
}

// TestRegistry_RequireTier_MediumNoOp asserts the medium tier is
// always on — no env var required, no Skip emitted.
func TestRegistry_RequireTier_MediumNoOp(t *testing.T) {
	RequireTier(t, "medium")
	// If RequireTier had skipped, we would not reach this assert.
	assert.False(t, t.Skipped(), "RequireTier(medium) must not skip")
}

// TestRegistry_RequireTier_LargeSkipsByDefault asserts the large tier
// gate skips when MACHE_E2E_LARGE is unset. Uses a sub-test so we can
// inspect Skipped() without actually skipping the parent.
func TestRegistry_RequireTier_LargeSkipsByDefault(t *testing.T) {
	// Ensure the env var is unset for the sub-test even if a parent
	// CI shell sets it.
	t.Setenv("MACHE_E2E_LARGE", "")
	_ = os.Unsetenv("MACHE_E2E_LARGE")

	// Capture the sub *testing.T to query Skipped() after t.Run returns
	// — Skipf calls runtime.Goexit so any post-call statement in the
	// closure never runs. Pointer capture is safe because t.Run blocks
	// until the sub-test finishes.
	var sub *testing.T
	t.Run("large-no-env", func(s *testing.T) {
		sub = s
		RequireTier(s, "large")
	})
	require.NotNil(t, sub, "sub-test must have run")
	assert.True(t, sub.Skipped(), "RequireTier(large) must skip when MACHE_E2E_LARGE is unset")
}

// TestRegistry_Get_MediumRustRosary proves the external snapshot
// added in ADR-0019 PR 2 (the medium-rust-rosary fixture) loads
// through the registry end-to-end: manifest entry resolves, rust
// preset schema parses, tree-sitter ingest produces a non-trivial
// SQLiteGraph. The 10-node floor catches the failure mode where the
// fixture path resolves but the snapshot is empty (which would
// otherwise pass an existence check and fail downstream tests with
// less actionable messages).
func TestRegistry_Get_MediumRustRosary(t *testing.T) {
	if testing.Short() {
		t.Skip("ingest is multi-second; rerun without -short")
	}
	t.Setenv("MACHE_NO_LEYLINE", "1")

	g := Get(t, "medium-rust-rosary")
	require.NotNil(t, g, "Get must return a non-nil SQLiteGraph for medium-rust-rosary")

	children, err := g.ListChildren("")
	require.NoError(t, err, "ListChildren must succeed on the projected root")
	require.GreaterOrEqual(t, len(children), 10,
		"medium-rust-rosary root must project ≥10 top-level entries; got %d (snapshot may be empty)",
		len(children))
}

// TestRegistry_All_ContainsMediumRustRosary is the manifest-shape
// smoke test for the rosary snapshot: All() must return an entry
// whose declared fields match what the curation tooling wrote.
func TestRegistry_All_ContainsMediumRustRosary(t *testing.T) {
	entries := All()
	require.NotEmpty(t, entries, "manifest must parse at least one fixture")

	var found bool
	for _, f := range entries {
		if f.ID == "medium-rust-rosary" {
			found = true
			assert.Equal(t, "medium", f.Tier)
			assert.Equal(t, "rust", f.Language)
			assert.Equal(t, "rust", f.SchemaPreset)
			assert.Equal(t, "medium-rust-rosary", f.Path)
			assert.Equal(t, "own", f.License)
			assert.NotEmpty(t, f.SHA, "medium-rust-rosary must record an upstream SHA")
			break
		}
	}
	assert.True(t, found, "manifest must contain the medium-rust-rosary entry")
}

// TestRegistry_All_ContainsMacheSelf is a smoke test: All() must
// return the mache-self entry parsed from manifest.toml.
func TestRegistry_All_ContainsMacheSelf(t *testing.T) {
	entries := All()
	require.NotEmpty(t, entries, "manifest must parse at least one fixture")

	var found bool
	for _, f := range entries {
		if f.ID == "mache-self" {
			found = true
			assert.Equal(t, "medium", f.Tier)
			assert.Equal(t, "go", f.Language)
			assert.Equal(t, "self", f.Source)
			assert.Equal(t, "$REPO_ROOT", f.Path)
			assert.Equal(t, "go", f.SchemaPreset)
			assert.Equal(t, "own", f.License)
			break
		}
	}
	assert.True(t, found, "manifest must contain the mache-self sentinel entry")
}
