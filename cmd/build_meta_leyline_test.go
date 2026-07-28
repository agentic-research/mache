package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// Regression for mache-438104.
//
// A persisted .db's node addresses depend on which leyline produced it: LLO
// v0.11.0 bumped IR_SCHEMA_VERSION merkle-ast-v1 -> v2, rewriting node_hash for
// Rust trait signatures from BYTE-IDENTICAL sources. mache's staleness checks
// are all content- or time-based (cache lockfiles hash source bytes, the parse
// skip is mtime+size), so none can observe it. Before this, _mache_meta recorded
// nothing about leyline and two .dbs from identical sources under different
// lineages were indistinguishable.
//
// These pin the two properties that make the artifact self-describing: the pin
// is ALWAYS recorded, and the resolved version is recorded when known and
// explicitly marked when not.

func readMeta(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT key, value FROM _mache_meta`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		require.NoError(t, rows.Scan(&k, &v))
		out[k] = v
	}
	require.NoError(t, rows.Err())
	return out
}

// The pin must be present unconditionally. It is compile-time, so there is no
// circumstance where mache cannot answer "which leyline did this build
// require?" — and an artifact that cannot answer that is the failure this bead
// is about.
func TestBuildMetadata_AlwaysRecordsLeylinePin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	require.NoError(t, writeBuildMetadata(dbPath, "leyline"))

	meta := readMeta(t, dbPath)
	assert.Equal(t, leyline.PinnedBinaryVersion(), meta["leyline_pin"],
		"leyline_pin must record the compile-time pin so the artifact is self-describing")
	assert.NotEmpty(t, meta["leyline_pin"])

	// The pre-existing keys must survive the addition.
	assert.Equal(t, "leyline", meta["backend"])
	assert.NotEmpty(t, meta["built_at"])
}

// When no binary was resolved this process, the key is still written with an
// explicit marker. Omitting it would make "built without leyline" look identical
// to "built by an older mache that did not stamp this" — a consumer could not
// distinguish absence-of-fact from absence-of-feature.
func TestBuildMetadata_RecordsUnresolvedRatherThanOmitting(t *testing.T) {
	if _, ok := leyline.Provenance(); ok {
		t.Skip("a binary was resolved in this process; cannot exercise the unresolved path")
	}
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	require.NoError(t, writeBuildMetadata(dbPath, "leyline"))

	meta := readMeta(t, dbPath)
	v, present := meta["leyline_version"]
	assert.True(t, present, "the key must be written even when nothing was resolved")
	assert.Equal(t, "unresolved", v)
}

// The resolved version must be the one that actually RAN, not the pin. These
// differ whenever MACHE_LEYLINE_BINARY points at a local build — exactly when
// knowing the difference matters, because the .db may not match what CI
// produces.
//
// Exercised through the pure form so it neither depends on a leyline binary
// being installed nor mutates the process-global provenance record.
func TestLeylineMetaRows_ResolvedVersionIsRecordedSeparatelyFromPin(t *testing.T) {
	prov := leyline.LeylineProvenance{
		Path:            "/tmp/leyline-dev",
		Version:         "leyline 0.11.0 (open)",
		Source:          "PATH",
		ExpectedVersion: "v0.10.4",
	}
	got := map[string]string{}
	for _, kv := range leylineMetaRowsFrom(prov, true, "v0.10.4") {
		got[kv[0]] = kv[1]
	}

	assert.Equal(t, "leyline 0.11.0 (open)", got["leyline_version"],
		"must record what ran, not what was required")
	assert.Equal(t, "v0.10.4", got["leyline_pin"],
		"the pin is independent of what was resolved and must not be overwritten")
	assert.Equal(t, "PATH", got["leyline_source"])

	assert.NotEqual(t, got["leyline_pin"], got["leyline_version"],
		"this fixture is the override case on purpose: a divergence between "+
			"pin and resolved version is the diagnostic signal, so both must be "+
			"recorded independently")
}

// An unresolved build records the marker rather than dropping the key, so a
// consumer can tell "built without leyline" from "built by an older mache that
// did not stamp this".
func TestLeylineMetaRows_UnresolvedIsExplicit(t *testing.T) {
	got := map[string]string{}
	for _, kv := range leylineMetaRowsFrom(leyline.LeylineProvenance{}, false, "v0.10.4") {
		got[kv[0]] = kv[1]
	}
	assert.Equal(t, "unresolved", got["leyline_version"])
	assert.Equal(t, "v0.10.4", got["leyline_pin"], "the pin is known even with no binary")
	_, hasSource := got["leyline_source"]
	assert.False(t, hasSource, "no source tier to report when nothing was resolved")
}

// ok=true with an empty Version is the degenerate case: a binary was resolved
// but `--version` failed. Treated as unresolved, because an empty string in the
// artifact is worse than an explicit marker.
func TestLeylineMetaRows_EmptyVersionTreatedAsUnresolved(t *testing.T) {
	got := map[string]string{}
	for _, kv := range leylineMetaRowsFrom(leyline.LeylineProvenance{Path: "/x"}, true, "v0.10.4") {
		got[kv[0]] = kv[1]
	}
	assert.Equal(t, "unresolved", got["leyline_version"],
		"a binary whose version could not be read must not stamp an empty string")
}
