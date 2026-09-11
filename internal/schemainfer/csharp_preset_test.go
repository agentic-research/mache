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

// recordPositionalProperties counts the parameters in a record's positional
// list — the properties C# generates that have no property_declaration node of
// their own. Matched on the node_id path, which is how the _ast records
// structure, the same way the smell rules discriminate on it.
func recordPositionalProperties(t *testing.T, astDB *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, astDB.QueryRow(
		`SELECT count(*) FROM _ast WHERE node_kind = 'parameter' AND node_id LIKE '%record_declaration%'`).Scan(&n))
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
		[]string{
			"Catalog.Capacity", "Catalog.Count", "Entry.Id", "Entry.Kind", "ILookup.Count",
			// A record's positional parameters ARE its properties in C#; the
			// compiler generates an init-only property per parameter. They
			// carry no property_declaration node, so a selector that only
			// looks for one leaves the whole public surface of a positional
			// record invisible (mache-daacc3).
			"Point.Total", "Point.X", "Point.Y", "Span.Hi", "Span.Lo",
		},
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
	assert.Equal(t,
		declaredCount(t, astDB, "property_declaration")+recordPositionalProperties(t, astDB),
		len(projectedNames(t, store, "properties")),
		"a declared property reached no selector")

	// The member's source is the member, not the type that declares it.
	src := projectedSource(t, store, "methods/ILookup.Lookup/source")
	assert.Contains(t, src, "Lookup(string id)")
	assert.NotContains(t, src, "interface ILookup")
}

// TestCsharpPreset_RecordsAreProjected pins mache-daacc3. `record` has been a
// first-class C# type declaration since C# 9 and is idiomatic for DTOs and
// value types, but the preset had no container for it: a record's members
// projected while the record itself did not exist, so an agent that found
// `methods/Point.Manhattan` and looked for the type declaring it found
// nothing, and `get_overview` showed a codebase with no record types in it.
//
// `record struct` produces a record_declaration too, so both forms land here.
func TestCsharpPreset_RecordsAreProjected(t *testing.T) {
	astDB, parseDir := lltest.ParseSourceViaLeyline(t, filepath.Join(testutil.PresetFixturesDir(t), "csharp"))
	store := projectPreset(t, "csharp", astDB, parseDir)

	assert.Equal(t, []string{"Point", "Span"}, projectedNames(t, store, "records"),
		"`record Point` and `record struct Span` are both record declarations")
	assert.Equal(t, declaredCount(t, astDB, "record_declaration"), len(projectedNames(t, store, "records")))

	// A record is not a class and must not be filed as one — the id would lie
	// about what the type is.
	for _, name := range projectedNames(t, store, "classes") {
		assert.NotContains(t, []string{"Point", "Span"}, name, "%s is a record, not a class", name)
	}

	assert.Contains(t, projectedSource(t, store, "records/Point/source"), "record Point(int X, int Y)")
}
