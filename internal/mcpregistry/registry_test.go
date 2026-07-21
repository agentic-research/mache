package mcpregistry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolRegistryNamesAreUniqueAndNonEmpty pins the basic invariants:
// every entry has a non-empty name and no name repeats.
func TestToolRegistryNamesAreUniqueAndNonEmpty(t *testing.T) {
	seen := map[string]struct{}{}
	for _, tool := range ToolRegistry() {
		require.NotEmpty(t, tool.Name, "tool name must be non-empty")
		_, dup := seen[tool.Name]
		require.False(t, dup, "duplicate tool name: %q", tool.Name)
		seen[tool.Name] = struct{}{}
	}
}

// TestCloisterGroupsCoverEveryRegisteredToolExactlyOnce is the coverage
// policy from bead `mache-802d2b`: every tool returned by
// ToolRegistry() MUST appear in exactly one group's UpstreamNames.
// Adding a tool without claiming it in CloisterGroups() fails CI here
// (and at server-json-gen runtime); removing a tool without pruning its
// group entry fails here too.
func TestCloisterGroupsCoverEveryRegisteredToolExactlyOnce(t *testing.T) {
	registered := map[string]struct{}{}
	for _, tool := range ToolRegistry() {
		registered[tool.Name] = struct{}{}
	}

	// Build owner map: tool -> list of groups that claim it.
	owner := map[string][]string{}
	groupNames := map[string]struct{}{}
	for _, g := range CloisterGroups() {
		require.NotEmpty(t, g.Name, "group `name` must be non-empty")
		require.NotEmpty(t, g.UpstreamNames,
			"group %q has empty UpstreamNames — spec violation", g.Name)
		_, dupGroup := groupNames[g.Name]
		require.False(t, dupGroup, "duplicate cloister group name: %q", g.Name)
		groupNames[g.Name] = struct{}{}

		for _, tool := range g.UpstreamNames {
			owner[tool] = append(owner[tool], g.Name)
		}
	}

	// Every registered tool is claimed.
	for tool := range registered {
		_, ok := owner[tool]
		assert.True(t, ok,
			"tool %q is in ToolRegistry() but no cloister group claims it",
			tool)
	}

	// Every claim points to a real tool; no tool is claimed by more
	// than one group.
	for tool, names := range owner {
		_, real := registered[tool]
		assert.True(t, real,
			"group claim %q is not in ToolRegistry()", tool)
		assert.Len(t, names, 1,
			"tool %q claimed by multiple groups: %v", tool, names)
	}
}

// TestCloisterGroupAdvertisedPrefixIsMachePrefix sanity-checks that
// every group advertises its tools under the `mache_` prefix — that's
// the prefix cloister's `[[routes.mcp.backends]]` declaration assigns
// to this upstream (handlesPrefix = "mache_", stripPrefix = "mache_").
// A future bundle could in principle split mache across multiple
// prefixes, but the current cloister wiring assumes one prefix per
// upstream — keep the invariant load-bearing until that changes.
func TestCloisterGroupAdvertisedPrefixIsMachePrefix(t *testing.T) {
	for _, g := range CloisterGroups() {
		assert.Equal(t, "mache_", g.AdvertisedPrefix,
			"group %q advertises under %q; cloister wiring expects mache_",
			g.Name, g.AdvertisedPrefix)
	}
}

// TestToolTiersAreValid guards the tier vocabulary.
//
// Tier is a string type, so a typo ("lsp " / "standalone") would compile and
// then be emitted straight into the published server.json. "standalone" in
// particular must never come back: it described the pre-v0.18.0 world where
// mache had an in-process parser, and ADR-0012 step 4 removed that — every
// source projection now requires ley-line-open.
func TestToolTiersAreValid(t *testing.T) {
	valid := map[Tier]bool{
		TierBase:       true,
		TierLSP:        true,
		TierEmbeddings: true,
		TierAny:        true,
	}
	for _, tool := range ToolRegistry() {
		resolved := tool.Tier.Resolved()
		if !valid[resolved] {
			t.Errorf("tool %q has unknown tier %q — valid tiers are base, lsp, embeddings, any",
				tool.Name, resolved)
		}
		if string(resolved) == "standalone" {
			t.Errorf("tool %q uses the retired %q tier: ley-line-open is required for "+
				"every source projection since v0.18.0", tool.Name, "standalone")
		}
	}
}
