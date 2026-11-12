package graph

import (
	"database/sql"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// metaDB builds the `_meta` key/value table ley-line-open writes. Shape copied
// from a real arena: CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL).
func metaDB(t *testing.T, rows map[string]string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if rows == nil {
		return db // no _meta table at all
	}
	_, err = db.Exec(`CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	require.NoError(t, err)
	for k, v := range rows {
		_, err = db.Exec(`INSERT INTO _meta (key, value) VALUES (?, ?)`, k, v)
		require.NoError(t, err)
	}
	return db
}

// TestProjectionVersion_ReportsWhatTheArtifactDeclares covers the three ways an
// artifact can decline to state its shape. They collapse to one answer because
// a reader cannot act on the difference between them.
//
// The no-_meta case is not hypothetical or defensive: it is EVERY currently
// released leyline. v0.18.0/v0.18.1/v0.18.2 have no PROJECTION_SCHEMA_VERSION
// constant at all, so the pinned binary mache ships against writes an arena
// with no projection row — as does mache's own `mache build`. Treating absence
// as an error would reject every artifact in existence today.
func TestProjectionVersion_ReportsWhatTheArtifactDeclares(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     map[string]string
		want     string
		declared bool
	}{
		{"no _meta table at all (mache build output)", nil, "", false},
		{
			"_meta without a projection row (every released leyline)",
			map[string]string{"ir_schema_version": "merkle-ast-v2", "parse_time": "1787339630"},
			"", false,
		},
		{
			// The _meta DDL is `value TEXT NOT NULL`, which permits the empty
			// string — NOT NULL is not non-empty. A row that exists but says
			// nothing is still an artifact declining to state its shape, and
			// must not be reported as a declared projection named "".
			"a projection row present but empty",
			map[string]string{ProjectionSchemaVersionKey: ""},
			"", false,
		},
		{
			"the pinned shape, declared",
			map[string]string{ProjectionSchemaVersionKey: "projection-v2"},
			"projection-v2", true,
		},
		{
			"projection-v4",
			map[string]string{ProjectionSchemaVersionKey: "projection-v4"},
			"projection-v4", true,
		},
		{
			"a shape from the future",
			map[string]string{ProjectionSchemaVersionKey: "projection-v9"},
			"projection-v9", true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, declared := ProjectionVersion(metaDB(t, tc.rows))
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.declared, declared)
		})
	}
}

// TestKnownProjections_CoverTheShapesMacheActuallyReads guards the set itself.
// It is the only thing standing between "mache was checked against this shape"
// and "nobody looked", so an entry going missing must fail rather than silently
// turn a shape mache does in fact support into a refusal.
func TestKnownProjections_CoverTheShapesMacheActuallyReads(t *testing.T) {
	for _, v := range []string{"projection-v2", "projection-v4", "projection-v6", "projection-v7", "projection-v8", "projection-v9", "projection-v10", "projection-v11", "projection-v12", "projection-v13"} {
		_, ok := knownProjections[v]
		assert.True(t, ok, "%s is a shape mache has been checked against", v)
	}
	assert.NotContains(t, knownProjections, "projection-v3",
		"projection-v3 never shipped: ley-line-open's two changes each wrote that string "+
			"independently and it became v4 before release, so claiming it would name a "+
			"shape that never existed")

	_, future := knownProjections["projection-v14"]
	assert.False(t, future, "the set must not be a wildcard")
}

// TestCheckProjection_RefusesOnlyWhatMacheHasNotRead pins both halves of the
// check. It runs on every Open, including a library caller assembling many
// projections, so an error on the normal path would make every known shape
// unreadable; and a version of it that passed everything would read a
// projection-v14 db as v13 and misfold its comment classes (mache-86e215).
func TestCheckProjection_RefusesOnlyWhatMacheHasNotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   map[string]string
		refuse bool
	}{
		{"no _meta (mache build)", nil, false},
		{"released leyline, no projection row", map[string]string{"parse_time": "1"}, false},
		{"a projection row present but empty", map[string]string{ProjectionSchemaVersionKey: ""}, false},
		{"a shape from the future", map[string]string{ProjectionSchemaVersionKey: "projection-v14"}, true},
		{"a fork's own string", map[string]string{ProjectionSchemaVersionKey: "acme-projection-1"}, true},
		{"projection-v3, which never shipped", map[string]string{ProjectionSchemaVersionKey: "projection-v3"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckProjection(metaDB(t, tc.rows))
			if !tc.refuse {
				assert.NoError(t, err, "must open a shape mache supports")
				return
			}
			require.ErrorIs(t, err, ErrUnknownProjection,
				"callers match the refusal by sentinel, not by message")
			assert.Contains(t, err.Error(), `"`+tc.rows[ProjectionSchemaVersionKey]+`"`,
				"the refusal must name the shape that was found")
			assert.Contains(t, err.Error(), "projection-v13",
				"and what mache does know, so the reader can judge the gap")
			assert.Contains(t, err.Error(), "mache leyline path",
				"and how to get a db this mache reads")
		})
	}
	// Every known shape, by the table rather than a hand-kept list, so adding
	// an entry cannot leave it untested.
	for _, v := range slices.Sorted(maps.Keys(knownProjections)) {
		t.Run(v, func(t *testing.T) {
			assert.NoError(t, CheckProjection(metaDB(t, map[string]string{ProjectionSchemaVersionKey: v})))
		})
	}
}

// leylineFixture is one leyline-shaped db with a construct in it, the artifact
// the open tests declare projections on.
func leylineFixture(t *testing.T) (string, *fixturedb.FixtureDB) {
	t.Helper()
	return fixturedb.New(t, fixturedb.Leyline).
		Def("F", "a.go/function_declaration_0", "function").
		Build()
}

// TestOpen_RefusesAnUnknownProjection is the refusal at the one place every
// reader of a .db goes through. No graph comes back: a partial graph over a
// shape mache does not know is the silent failure this replaces.
func TestOpen_RefusesAnUnknownProjection(t *testing.T) {
	path, f := leylineFixture(t)
	fixturedb.DeclareProjection(t, f.DB(), "projection-v14")

	g, err := Open(path)
	require.ErrorIs(t, err, ErrUnknownProjection)
	assert.Nil(t, g, "no graph is returned for a refused db")
	assert.Contains(t, err.Error(), path, "the error names the artifact, since a caller may hold many")
	assert.Contains(t, err.Error(), `"projection-v14"`)

	wg, err := OpenWritableGraph(path, &api.Topology{}, stubRender, nil)
	require.ErrorIs(t, err, ErrUnknownProjection, "the writable open reads the same tables and refuses the same")
	assert.Nil(t, wg)
}

// TestOpen_AcceptsEveryKnownProjectionAndNone is the other half: the refusal
// is for unknown shapes only. An undeclared db (every pre-_meta arena and
// mache's own build) and every shape in knownProjections open and answer.
func TestOpen_AcceptsEveryKnownProjectionAndNone(t *testing.T) {
	open := func(t *testing.T, path string) {
		t.Helper()
		g, err := Open(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close() })
		assert.NotEmpty(t, g.LookupDef("F"), "an opened graph answers")
	}
	t.Run("undeclared", func(t *testing.T) {
		path, f := leylineFixture(t)
		_, declared := ProjectionVersion(f.DB())
		require.False(t, declared, "the fixture declares nothing unless asked to")
		open(t, path)
	})
	for _, v := range slices.Sorted(maps.Keys(knownProjections)) {
		t.Run(v, func(t *testing.T) {
			path, f := leylineFixture(t)
			fixturedb.DeclareProjection(t, f.DB(), v)
			got, declared := ProjectionVersion(f.DB())
			require.True(t, declared)
			require.Equal(t, v, got, "DeclareProjection writes the row ProjectionVersion reads")
			open(t, path)
		})
	}
}
