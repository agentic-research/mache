package schemainfer

import (
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRubyPreset_EveryMethodHasOneDeclarer pins the other half of
// mache-34c926.
//
// Ruby nests methods under `classes/<Class>/methods` correctly, but the
// TOP-LEVEL `methods/` container's selector matched a `method` node anywhere,
// including inside a class body. So every class method was projected TWICE —
// once correctly, and once at the top level where two classes sharing a
// method name collided into `count` and `count.from_catalog_rb`. Five methods
// in the fixture produced seven nodes, and every one of them was
// double-counted in node_defs.
//
// The top-level container is now scoped to program-level defs. `modules/`
// gained the `methods/` container `classes/` already had, so scoping the
// top-level one does not make a module's methods unreachable — that would
// trade a duplicate for a disappearance.
func TestRubyPreset_EveryMethodHasOneDeclarer(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, filepath.Join(testutil.PresetFixturesDir(t), "ruby"))
	store := projectPreset(t, "ruby", astDB, parseDir)

	assert.Equal(t, []string{"count", "insert"}, projectedNames(t, store, "classes/Catalog/methods"))
	assert.Equal(t, []string{"count"}, projectedNames(t, store, "classes/Index/methods"))
	assert.Equal(t, []string{"register"}, projectedNames(t, store, "modules/Registry/methods"),
		"a module's methods must still be reachable")
	assert.Equal(t, []string{"top_level_helper"}, projectedNames(t, store, "methods"),
		"the top-level container holds program-level defs only")

	// The counting assertion: one node per method, no more. Before the fix
	// this was 5 parsed against 7 projected.
	projected := 0
	for _, dir := range []string{
		"classes/Catalog/methods", "classes/Index/methods",
		"modules/Registry/methods", "methods",
	} {
		projected += len(projectedNames(t, store, dir))
	}
	declared := declaredCount(t, astDB, "method")
	t.Logf("%d method nodes; %d projected", declared, projected)
	assert.Equal(t, declared, projected,
		"%d methods parsed but %d projected; a method is either duplicated or missing", declared, projected)

	// Two classes declaring `count` are two different methods, and neither
	// wears a filename suffix to say so.
	for _, id := range []string{"classes/Catalog/methods/count", "classes/Index/methods/count"} {
		_, err := store.GetNode(id)
		require.NoError(t, err, "%s must exist", id)
	}
	assert.NotContains(t, projectedSource(t, store, "classes/Index/methods/count/source"), "def insert")
}
