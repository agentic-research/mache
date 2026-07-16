package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// seedCoverageFixture builds a synthetic leyline-parse db containing _ast
// rows ONLY for Go, plus a source dir holding both a .go and a .sql file —
// the exact hollow-projection shape the review of mache-73b885 reproduced
// live (as of ley-line-open v0.8.0 only CUE lacks a grammar; sql/java/etc.
// all parse now, so cue is the sole hollow-projection language)
// empty with zero warnings).
func seedCoverageFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "parse.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE _ast (
		node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL,
		start_byte INTEGER, end_byte INTEGER, start_row INTEGER, start_col INTEGER,
		end_row INTEGER, end_col INTEGER, node_hash BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO _ast (node_id, source_id, node_kind) VALUES
		('main.go/function_declaration', 'main.go', 'function_declaration')`)
	require.NoError(t, err)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "util.sql"),
		[]byte("CREATE TABLE users (id INTEGER);\n"), 0o644))
	return db, src
}

func TestLeylineSchemaCoverageGaps(t *testing.T) {
	db, src := seedCoverageFixture(t)

	schemaFor := func(langs ...string) *api.Topology {
		topo := &api.Topology{Version: "v1alpha1"}
		for _, l := range langs {
			topo.Nodes = append(topo.Nodes, api.Node{Name: l, Language: l})
		}
		return topo
	}

	// sql files exist, zero sql _ast rows → gap. go parsed fine → no gap.
	gaps, err := leylineSchemaCoverageGaps(db, schemaFor("go", "sql"), src, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps,
		"sql has source files but no _ast rows; go parsed")

	// A schema language with NO source files is not a gap — nothing to
	// project, identical to the tree-sitter backend's behavior.
	gaps, err = leylineSchemaCoverageGaps(db, schemaFor("go", "rust"), src, nil)
	require.NoError(t, err)
	assert.Empty(t, gaps, "no .rs files in source — absence is not a gap")

	// Language hints on CHILD nodes are collected too.
	nested := &api.Topology{Version: "v1alpha1", Nodes: []api.Node{
		{Name: "root", Children: []api.Node{{Name: "tables", Language: "sql"}}},
	}}
	gaps, err = leylineSchemaCoverageGaps(db, nested, src, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps)

	// No Language hints anywhere → guard opts out entirely.
	gaps, err = leylineSchemaCoverageGaps(db, &api.Topology{
		Version: "v1alpha1",
		Nodes:   []api.Node{{Name: "anything"}},
	}, src, nil)
	require.NoError(t, err)
	assert.Nil(t, gaps)

	// Unknown language string is ignored (matches engine behavior).
	gaps, err = leylineSchemaCoverageGaps(db, schemaFor("klingon"), src, nil)
	require.NoError(t, err)
	assert.Empty(t, gaps)

	// extraLangs covers the preset-ref case (#524 re-review): preset
	// schemas carry ZERO Language hints, so a hint-less topology plus
	// extraLangs=["sql"] must still report the gap.
	gaps, err = leylineSchemaCoverageGaps(db, &api.Topology{
		Version: "v1alpha1",
		Nodes:   []api.Node{{Name: "tables"}},
	}, src, []string{"sql"})
	require.NoError(t, err)
	assert.Equal(t, []string{"sql"}, gaps,
		"a hint-less schema with a preset-derived language must still gap")
}

// TestRunBuildViaLeylineSchema_UnparseableLanguageErrors is the e2e pin for
// the review finding: an EXPLICIT --backend=leyline build whose schema
// targets a language the pinned leyline can't parse must ERROR (not emit a
// hollow schema-shaped db), naming the language. The auto path downgrades
// to a warning — same loudness split the pre-73b885 code had.
func TestRunBuildViaLeylineSchema_UnparseableLanguageErrors(t *testing.T) {
	requirePinnedLeyline(t)
	saved := saveBuildFlags()
	defer saved.restore()

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "conf.cue"),
		[]byte("package conf\n\nx: 1\n"), 0o644))
	output := filepath.Join(t.TempDir(), "out.db")

	// cue is the SOLE language ley-line-open v0.8.0 has no grammar for
	// (no tree-sitter-0.26 cue grammar exists) — the last hollow-projection
	// case after sql/java/etc. gained grammars. Preset ref (no Language
	// hints) exercises the preset-derived language stamp.
	schemaPath = "cue"
	err := runBuildViaLeyline(src, output, true /* explicit backend */)
	require.Error(t, err, "preset-ref hollow projection must not build silently on the explicit backend")
	assert.Contains(t, err.Error(), "cue", "error must name the unparseable language")
	assert.Contains(t, err.Error(), "--backend=tree-sitter", "error must point at the working escape hatch")

	// Same via an explicit Language-hinted schema FILE (the hint collector).
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "schema.json"),
		[]byte(`{"version":"v1alpha1","nodes":[{"name":"fields","selector":"(field) @f","language":"cue"}]}`), 0o644))
	t.Chdir(work)
	schemaPath = "schema.json"
	err = runBuildViaLeyline(src, output, true /* explicit backend */)
	require.Error(t, err, "hint-based hollow projection must not build silently on the explicit backend")
	assert.Contains(t, err.Error(), "cue")

	// Auto path: warns and builds (advisory), does not error.
	require.NoError(t, runBuildViaLeyline(src, output, false /* auto */),
		"auto path degrades with a warning, not an error")
}
