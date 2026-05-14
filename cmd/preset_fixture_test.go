package cmd

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// presetFixtureCase pairs a preset name with a fixture directory and
// the node-path substrings that must appear in the resulting graph.
// TestPresetSchemas_AgainstFixtures runs each case end-to-end:
//
//	loadPresetSchema → ingest.NewEngine → engine.Ingest → walk the
//	resulting MemoryStore and grep for expected substrings.
//
// This complements TestPresetSchemas_SelectorsCompile, which only
// confirms selectors compile against the grammar. A selector can
// compile cleanly and still match zero rows — e.g. wrong field name,
// wrong nesting depth. Real-fixture tests catch that class of bug.
//
// Adding a new case:
//  1. Drop fixture files under cmd/testdata/preset_fixtures/<name>/
//  2. Append an entry below with the expected node-path substrings
//  3. Run `go test -run TestPresetSchemas_AgainstFixtures/<name>`
type presetFixtureCase struct {
	// preset is the registered preset name (matches loadPresetSchema).
	preset string
	// fixtureDir is the subdir under cmd/testdata/preset_fixtures/ that
	// holds the source files this case ingests.
	fixtureDir string
	// expectedSubstrings is a list of substrings that must appear in
	// some node ID in the resulting graph. Substrings (not exact
	// equality) so the test survives leaf-name conventions changes
	// (e.g. functions/foo/source vs functions/foo).
	expectedSubstrings []string
	// minNodes is the floor on total ingested node count. Sanity check
	// against "selectors silently matched nothing — graph is empty".
	minNodes int
}

func presetFixtureCases(t *testing.T) []presetFixtureCase {
	t.Helper()
	// Fixture dir base lives next to this file. runtime.Caller picks
	// the test file path so the lookup works regardless of `go test
	// -C` or vendored paths.
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) returned ok=false")
	fixturesRoot := filepath.Join(filepath.Dir(thisFile), "testdata", "preset_fixtures")

	// Go: use a small, stable subdir of mache itself as the fixture.
	// internal/lang has a single package with the language registry,
	// init function, constants, and a couple of helpers — exercises
	// imports/functions/types/constants/variables. Path is computed
	// relative to this test file so the fixture moves with the repo.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	goFixture := filepath.Join(repoRoot, "internal", "lang")

	return []presetFixtureCase{
		{
			preset:     "go",
			fixtureDir: goFixture,
			// Anchored against internal/lang/: a tightly-scoped real
			// fixture (mache's own language registry). If any of these
			// disappear, either the schema regressed or internal/lang
			// got refactored — both are worth a failing test.
			expectedSubstrings: []string{
				"lang/imports/",                 // imports surface
				"lang/functions/init",           // package init() function
				"lang/functions/ForExt",         // exported helper
				"lang/functions/enrichHCLNode",  // unexported helper
				"lang/types",                    // types directory exists
			},
			minNodes: 50,
		},
		{
			preset:     "rust",
			fixtureDir: filepath.Join(fixturesRoot, "rust"),
			// The rust preset surfaces structs/enums/traits/functions/
			// impls/imports/modules/constants/statics — the lib.rs
			// fixture exercises each. Substring anchored on the
			// expected directory name so this catches "selector
			// matched the right thing but wrapped it in the wrong
			// parent".
			expectedSubstrings: []string{
				"structs/Catalog",
				"structs/Entry",
				"enums/EntryKind",
				"traits/Lookup",
				"functions/open",
				"constants/DEFAULT_CAPACITY",
				"statics/GLOBAL_NAME",
				"modules/parser",
				"imports/std::collections::HashMap",
				"implementations/Catalog",
			},
			minNodes: 30,
		},
		{
			preset:     "terraform",
			fixtureDir: filepath.Join(fixturesRoot, "terraform"),
			// Block-types the HCL preset distinguishes via the
			// (#eq? @_type "...") predicates: resource/data/variable/
			// output/module/local/provider/terraform. The fixture
			// has at least one of each (data block has empty body and
			// won't produce a child entry, but the parent dir still
			// exists).
			expectedSubstrings: []string{
				`resources/"artifacts"`,
				`variables/"region"`,
				`variables/"bucket_name"`,
				`outputs/"bucket_arn"`,
				`outputs/"account_id"`,
				`modules/"logging"`,
				`providers/"aws"`,
				"locals/locals",
				"terraform/terraform",
			},
			minNodes: 20,
		},
	}
}

// TestPresetSchemas_AgainstFixtures asserts every preset case ingests
// its fixture and produces nodes whose IDs include the expected path
// substrings. Failure mode the test catches: a selector compiles but
// matches zero rows of real source, so the graph is empty.
func TestPresetSchemas_AgainstFixtures(t *testing.T) {
	for _, tc := range presetFixtureCases(t) {
		t.Run(tc.preset, func(t *testing.T) {
			schema, err := loadPresetSchema(tc.preset)
			require.NoError(t, err, "load preset %q", tc.preset)

			store := graph.NewMemoryStore()
			engine := ingest.NewEngine(schema, store)
			require.NoError(t, engine.Ingest(tc.fixtureDir),
				"engine.Ingest(%s) for preset %q", tc.fixtureDir, tc.preset)

			ids := collectAllNodeIDs(t, store)
			if testing.Verbose() {
				t.Logf("preset %q produced %d node IDs:\n  %s",
					tc.preset, len(ids), sampleIDs(ids, 50))
			}
			assert.GreaterOrEqualf(t, len(ids), tc.minNodes,
				"preset %q produced %d nodes from %s, expected >= %d "+
					"(selectors compile but may match nothing)",
				tc.preset, len(ids), tc.fixtureDir, tc.minNodes)

			for _, want := range tc.expectedSubstrings {
				if !containsSubstring(ids, want) {
					t.Errorf("preset %q: no node ID contains %q\n"+
						"first 20 ids: %s",
						tc.preset, want, sampleIDs(ids, 20))
				}
			}
		})
	}
}

// collectAllNodeIDs walks the store from each root, depth-first, and
// returns every node ID it finds. Doesn't deduplicate (the graph is
// already a tree, so duplicates aren't expected); ordering is
// undefined.
func collectAllNodeIDs(t *testing.T, store *graph.MemoryStore) []string {
	t.Helper()
	var ids []string
	var walk func(id string)
	walk = func(id string) {
		ids = append(ids, id)
		children, err := store.ListChildren(id)
		if err != nil {
			return
		}
		for _, c := range children {
			walk(c)
		}
	}
	for _, root := range store.RootIDs() {
		walk(root)
	}
	return ids
}

func containsSubstring(ids []string, sub string) bool {
	for _, id := range ids {
		if strings.Contains(id, sub) {
			return true
		}
	}
	return false
}

// sampleIDs returns a sorted, truncated slice of node IDs for use in
// failure messages. Helps the author of a new fixture case see what
// the engine actually produced.
func sampleIDs(ids []string, n int) string {
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return strings.Join(sorted, "\n  ")
}
