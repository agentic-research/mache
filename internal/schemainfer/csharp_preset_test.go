package schemainfer

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredCount is how many nodes of one AST kind ley-line parsed. Every
// "is each declaration projected exactly once" assertion in this package
// compares a projection against this.
func declaredCount(t *testing.T, astDB *sql.DB, nodeKind string) int {
	t.Helper()
	var n int
	require.NoError(t, astDB.QueryRow(
		`SELECT count(*) FROM _ast WHERE node_kind = ?`, nodeKind).Scan(&n))
	require.Positive(t, n, "no %s nodes in the fixture", nodeKind)
	return n
}

// TestCsharpPreset_MembersAreTypeQualified pins mache-34c926.
//
// C# projected methods and properties as flat `methods/<name>` and
// `properties/<name>`, so an interface member and its implementation
// collided. `ILookup.Lookup` and `Catalog.Lookup` became `methods/Lookup`
// and `methods/Lookup.from_Catalog_cs` — a filename suffix, which
// distinguishes nothing when both are declared in the same file and says
// nothing about which type owns either. Same for `ILookup.Count` and
// `Catalog.Count`.
//
// Every other class-based preset qualifies its methods by nesting them under
// the declaring type. C# is flat, so it takes the other working route, the
// one go and rust already use: a `<Type>.<name>` id.
func TestCsharpPreset_MembersAreTypeQualified(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, filepath.Join(testutil.PresetFixturesDir(t), "csharp"))
	store := projectPreset(t, "csharp", astDB, parseDir)

	assert.Equal(t,
		[]string{"Catalog.Insert", "Catalog.Lookup", "ILookup.Lookup", "Point.Manhattan", "Span.Width"},
		projectedNames(t, store, "methods"),
		"an interface method and its implementation are different methods, and a record declares methods too")
	assert.Equal(t,
		[]string{"Catalog.Capacity", "Catalog.Count", "Entry.Id", "Entry.Kind", "ILookup.Count", "Point.Total"},
		projectedNames(t, store, "properties"))

	for _, dir := range []string{"methods", "properties"} {
		for _, name := range projectedNames(t, store, dir) {
			assert.NotContains(t, name, ".from_",
				"%s/%s fell back to a filename suffix; the type qualification did not take", dir, name)
		}
	}

	// Every declaration is projected exactly once. Renaming members without
	// this would be free to drop one.
	assert.Equal(t, declaredCount(t, astDB, "method_declaration"), len(projectedNames(t, store, "methods")))
	assert.Equal(t, declaredCount(t, astDB, "property_declaration"), len(projectedNames(t, store, "properties")))

	// The member's source is the member, not the type that declares it.
	src := projectedSource(t, store, "methods/ILookup.Lookup/source")
	assert.Contains(t, src, "Lookup(string id)")
	assert.NotContains(t, src, "interface ILookup")
}
