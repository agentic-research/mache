package graph

import (
	"bytes"
	"database/sql"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
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
// widen the warning to shapes mache does in fact support.
func TestKnownProjections_CoverTheShapesMacheActuallyReads(t *testing.T) {
	for _, v := range []string{"projection-v2", "projection-v4"} {
		_, ok := knownProjections[v]
		assert.True(t, ok, "%s is a shape mache has been checked against", v)
	}
	assert.NotContains(t, knownProjections, "projection-v3",
		"projection-v3 never shipped: ley-line-open's two changes each wrote that string "+
			"independently and it became v4 before release, so claiming it would name a "+
			"shape that never existed")

	_, future := knownProjections["projection-v9"]
	assert.False(t, future, "the set must not be a wildcard")
}

// TestWarnUnknownProjection_IsQuietOnEverythingMacheSupports pins the property
// that decides whether this check is usable at all. It runs on every Open,
// including a library caller assembling many projections, so a warning on the
// normal path would be pure noise and would train readers to ignore it.
//
// Asserted via the log sink rather than by reading the function: "does it warn"
// is the whole behaviour, and a version of this that returned early for every
// input would pass any test that only checked the supported cases.
func TestWarnUnknownProjection_IsQuietOnEverythingMacheSupports(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     map[string]string
		wantWarn bool
	}{
		{"no _meta (mache build)", nil, false},
		{"released leyline, no projection row", map[string]string{"parse_time": "1"}, false},
		{"pinned shape", map[string]string{ProjectionSchemaVersionKey: "projection-v2"}, false},
		{"projection-v4", map[string]string{ProjectionSchemaVersionKey: "projection-v4"}, false},
		{"a shape from the future", map[string]string{ProjectionSchemaVersionKey: "projection-v9"}, true},
		{"a fork's own string", map[string]string{ProjectionSchemaVersionKey: "acme-projection-1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureWarnLog(t, func() {
				warnUnknownProjection(metaDB(t, tc.rows), "/tmp/some.db")
			})
			if !tc.wantWarn {
				assert.Empty(t, logged, "must stay silent on a shape mache supports")
				return
			}
			assert.Contains(t, logged, "warn:")
			assert.Contains(t, logged, tc.rows[ProjectionSchemaVersionKey],
				"the warning must name the shape that was found")
			assert.Contains(t, logged, "/tmp/some.db",
				"and which artifact declared it, since a caller may hold many")
			assert.Contains(t, logged, "projection-v4",
				"and what mache does know, so the reader can judge the gap")
		})
	}
}

// captureWarnLog redirects the standard logger for the duration of fn and returns
// what it wrote. warnUnknownProjection reports through log.Printf because it is
// advisory — there is no error to return from a path that deliberately does not
// fail — so the log IS the observable behaviour and has to be asserted directly.
func captureWarnLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	out, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	fn()
	return buf.String()
}
