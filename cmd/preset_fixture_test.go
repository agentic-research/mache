package cmd

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingFixtureCoverage lists presets that don't yet have a case in
// presetFixtureCases. Every entry is a TODO — either author the fixture
// (preferred) or update the value with the reason it can't be tested
// here. The companion test TestPresetSchemas_PendingFixtureCoverage
// fails when a registered preset is missing from BOTH this list and
// presetFixtureCases, AND when an entry here is also in
// presetFixtureCases (double-coverage means the TODO is stale), AND
// when an entry here no longer exists in presetSchemas (preset was
// removed but the TODO wasn't).
//
// Most of these are "rich" presets (>100 lines) that were in production
// before fixture coverage existed; they're exercised indirectly by
// other tests (TestInferDirSchema_*, the integration test suite, the
// dead_code rule's Go assertions). Authoring direct fixtures for them
// is straightforward but high-volume — pick one off, add a fixture
// under cmd/testdata/preset_fixtures/<name>/, append a case to
// presetFixtureCases, remove it from this map.
//
// Format: preset name → reason. Empty string means "no reason
// recorded, just hasn't been written yet."
var pendingFixtureCoverage = map[string]string{
	"c":          "",
	"cpp":        "",
	"elixir":     "",
	"java":       "",
	"javascript": "",
	"kotlin":     "",
	"php":        "",
	"python":     "",
	"ruby":       "",
	"scala":      "",
	"swift":      "",
	"typescript": "",
}

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
	// skip, when non-empty, skips this preset with the given reason. Used
	// for presets whose selectors don't yet project cleanly under leyline's
	// _ast (calibrated to the removed in-process tree-sitter) — tracked in
	// mache-384689 — and for cue, which leyline has no grammar for.
	skip string
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
				"lang/imports/",         // imports surface
				"lang/functions/init",   // package init() function
				"lang/functions/ForExt", // exported helper
				"lang/types",            // types directory exists
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
			skip:     "mache-384689 / ley-line-open-411f3c: leyline HCL grammar exposes only the FIRST block label, so resource/data NAMES are unrecoverable in _ast (type-only keying would collide). Blocked on the upstream grammar enhancement",
		},
		{
			preset:             "bash",
			fixtureDir:         filepath.Join(fixturesRoot, "bash"),
			expectedSubstrings: []string{"functions/log", "functions/ensure_dir", "if_statements/", "for_loops/", "case_statements/", "commands/"},
			minNodes:           10,
		},
		{
			preset:     "csharp",
			fixtureDir: filepath.Join(fixturesRoot, "csharp"),
			expectedSubstrings: []string{
				"namespaces/", "classes/Catalog", "classes/Entry",
				"structs/EntryStats", "interfaces/ILookup", "enums/EntryKind",
				"methods/Insert", "methods/Lookup",
			},
			minNodes: 10,
		},
		{
			preset:             "css",
			fixtureDir:         filepath.Join(fixturesRoot, "css"),
			expectedSubstrings: []string{"rules/", "media_queries/", "keyframes/", "imports/", "supports/"},
			minNodes:           8,
		},
		{
			preset:             "cue",
			fixtureDir:         filepath.Join(fixturesRoot, "cue"),
			expectedSubstrings: []string{"package/", "imports/", "fields/", "let_clauses/"},
			minNodes:           8,
			skip:               "mache-384689: leyline has no cue grammar (no tree-sitter-0.26 cue grammar exists anywhere)",
		},
		{
			preset:     "dockerfile",
			fixtureDir: filepath.Join(fixturesRoot, "dockerfile"),
			expectedSubstrings: []string{
				"stages/", "run_steps/", "copy_steps/",
				"env/", "workdir/", "expose/", "labels/", "healthcheck/",
			},
			minNodes: 8,
		},
		{
			preset:             "groovy",
			fixtureDir:         filepath.Join(fixturesRoot, "groovy"),
			expectedSubstrings: []string{"package/", "imports/", "classes/BuildConfig", "functions/"},
			minNodes:           5,
		},
		{
			preset:             "html",
			fixtureDir:         filepath.Join(fixturesRoot, "html"),
			expectedSubstrings: []string{"elements/", "scripts/", "styles/", "doctype/"},
			minNodes:           5,
		},
		{
			preset:     "lua",
			fixtureDir: filepath.Join(fixturesRoot, "lua"),
			expectedSubstrings: []string{
				"functions/", "locals/", "if_statements/",
				"for_statements/", "while_statements/",
			},
			minNodes: 10,
		},
		{
			preset:     "markdown",
			fixtureDir: filepath.Join(fixturesRoot, "markdown"),
			expectedSubstrings: []string{
				"sections/", "code_blocks/", "lists/",
				"paragraphs/", "block_quotes/", "tables/", "html_blocks/",
			},
			minNodes: 10,
		},
		{
			preset:     "protobuf",
			fixtureDir: filepath.Join(fixturesRoot, "protobuf"),
			expectedSubstrings: []string{
				"package/", "imports/",
				"messages/Entry", "messages/LookupRequest",
				"services/Catalog", "enums/EntryKind", "rpcs/",
			},
			minNodes: 8,
		},
		{
			preset:     "sql",
			fixtureDir: filepath.Join(fixturesRoot, "sql"),
			expectedSubstrings: []string{
				"tables/users", "tables/sessions",
				"views/active_sessions", "indexes/", "triggers/",
				"functions/", "types/",
			},
			minNodes: 10,
		},
		{
			preset:     "toml",
			fixtureDir: filepath.Join(fixturesRoot, "toml"),
			expectedSubstrings: []string{
				"tables/package", "tables/dependencies",
				"array_tables/bin", "pairs/", "inline_tables/",
			},
			minNodes: 10,
		},
		{
			preset:     "yaml",
			fixtureDir: filepath.Join(fixturesRoot, "yaml"),
			expectedSubstrings: []string{
				"mappings/", "documents/", "block_scalars/",
			},
			minNodes: 5,
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
			if tc.skip != "" {
				t.Skip(tc.skip)
			}
			schema, err := loadPresetSchema(tc.preset)
			require.NoError(t, err, "load preset %q", tc.preset)

			store := graph.NewMemoryStore()
			engine := ingest.NewEngine(schema, store)
			lltest.IngestSourceViaLeyline(t, engine, tc.fixtureDir)

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

// TestPresetSchemas_PendingFixtureCoverage is the patrol that catches
// new presets being added without fixture coverage. It computes the
// set of registered presets (presetSchemas keys, minus the data-format
// presets that don't use tree-sitter grammars), compares against
// what's in presetFixtureCases plus pendingFixtureCoverage, and fails
// when any registered preset shows up in neither — or when anything
// in pendingFixtureCoverage no longer matches presetSchemas.
//
// Failure mode the test catches: somebody adds a new language to
// internal/lang's Registry with a new preset schema, wires it through
// cmd/schemas/<name>.json, but forgets to author a fixture. Without
// this test the new preset would silently fall back to
// "selector compiles, but does it actually match real source?" being
// unverified.
//
// To unblock a failing run, do one of:
//
//   - Add a case to presetFixtureCases (preferred — author the fixture)
//   - Add the preset to pendingFixtureCoverage with a reason
//   - Remove the preset from pendingFixtureCoverage if it's no longer
//     registered or if you just added a case
func TestPresetSchemas_PendingFixtureCoverage(t *testing.T) {
	// Data-format presets use JSONPath against record-shaped data
	// (not tree-sitter against source files). Skipping them matches
	// TestPresetSchemas_SelectorsCompile's dataPresets carveout.
	dataPresets := map[string]bool{"cli": true, "mcp": true, "mcp-registry": true}

	covered := make(map[string]bool, len(presetFixtureCases(t)))
	for _, c := range presetFixtureCases(t) {
		covered[c.preset] = true
	}

	var (
		uncovered               []string
		pendingButCovered       []string
		pendingButNotInRegistry []string
	)

	for name := range presetSchemas {
		if dataPresets[name] {
			continue
		}
		_, pending := pendingFixtureCoverage[name]
		switch {
		case covered[name] && pending:
			pendingButCovered = append(pendingButCovered, name)
		case !covered[name] && !pending:
			uncovered = append(uncovered, name)
		}
	}

	for name := range pendingFixtureCoverage {
		if _, exists := presetSchemas[name]; !exists {
			pendingButNotInRegistry = append(pendingButNotInRegistry, name)
		}
	}

	sort.Strings(uncovered)
	sort.Strings(pendingButCovered)
	sort.Strings(pendingButNotInRegistry)

	if len(uncovered) > 0 {
		t.Errorf("preset(s) registered in presetSchemas have no fixture coverage and aren't on the pending allowlist:\n"+
			"  %s\n"+
			"Either add a case to presetFixtureCases (with a fixture under cmd/testdata/preset_fixtures/<name>/) "+
			"or list the preset in pendingFixtureCoverage with a reason.",
			strings.Join(uncovered, ", "))
	}
	if len(pendingButCovered) > 0 {
		t.Errorf("preset(s) are listed in pendingFixtureCoverage but also have a case in presetFixtureCases:\n"+
			"  %s\n"+
			"Remove them from pendingFixtureCoverage — they're already covered.",
			strings.Join(pendingButCovered, ", "))
	}
	if len(pendingButNotInRegistry) > 0 {
		t.Errorf("preset(s) listed in pendingFixtureCoverage are no longer in presetSchemas:\n"+
			"  %s\n"+
			"Remove them from pendingFixtureCoverage — they no longer exist.",
			strings.Join(pendingButNotInRegistry, ", "))
	}
}

// collectAllNodeIDs walks the store from each root, depth-first, and
// returns every node ID it finds. Doesn't deduplicate (the graph is
// already a tree, so duplicates aren't expected); ordering is
// undefined.
//
// ListChildren errors are FATAL: a partial traversal would silently
// mask graph-integrity bugs (broken edges, missing parents, store
// corruption) and let tests pass on a graph that's actually wrong.
// This helper exists to verify fixture coverage — a fixture test
// that "passes" because the walk gave up halfway is worse than
// useless.
func collectAllNodeIDs(t *testing.T, store *graph.MemoryStore) []string {
	t.Helper()
	var ids []string
	var walk func(id string)
	walk = func(id string) {
		ids = append(ids, id)
		children, err := store.ListChildren(id)
		require.NoError(t, err, "ListChildren(%q): graph integrity failure", id)
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
