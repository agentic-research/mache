package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/graph"
)

// writeOneDirNode builds a .db holding a single dir node that carries a string
// property (lang) and an object-valued one (imports), then returns its path.
func writeOneDirNode(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "props.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	n := &graph.Node{
		ID:      "pkg",
		Mode:    os.ModeDir | 0o555,
		ModTime: time.Unix(1700000000, 0),
	}
	graph.SetPropString(n, "lang", "go")
	graph.SetPropRaw(n, "imports", []byte(`{"fmt":"fmt"}`))
	w.AddNode(n)
	require.NoError(t, w.Close())
	return dbPath
}

func openRO(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestPropsColumnIsQueryableJSON is the point of mache-90b89b. Properties used
// to be base64'd into `record`, so json_extract returned "Z28=" instead of "go"
// and no SQL or smell rule could filter on lang/pkg/imports.
func TestPropsColumnIsQueryableJSON(t *testing.T) {
	db := openRO(t, writeOneDirNode(t))

	var lang string
	require.NoError(t, db.QueryRow(
		`SELECT json_extract(props,'$.lang') FROM nodes WHERE id='pkg'`).Scan(&lang))
	assert.Equal(t, "go", lang, "must be the value, not its base64")

	var importsType string
	require.NoError(t, db.QueryRow(
		`SELECT json_type(props,'$.imports') FROM nodes WHERE id='pkg'`).Scan(&importsType))
	assert.Equal(t, "object", importsType,
		"imports must be a nested object, not a base64 text blob")
}

// TestRecordNoLongerCarriesProperties pins the de-overloading. `record` used to
// mean a source data record OR inline content OR serialized Properties; the
// third meaning is gone.
func TestRecordNoLongerCarriesProperties(t *testing.T) {
	db := openRO(t, writeOneDirNode(t))

	var record sql.NullString
	require.NoError(t, db.QueryRow(`SELECT record FROM nodes WHERE id='pkg'`).Scan(&record))
	assert.False(t, record.Valid && record.String != "",
		"a dir node with no inline Data must leave record NULL; got %q", record.String)
}

// TestPropsStoresNoBase64 is the regression guard: if Properties ever reverts to
// map[string][]byte, json.Marshal silently base64s every value again and this
// is what catches it.
func TestPropsStoresNoBase64(t *testing.T) {
	db := openRO(t, writeOneDirNode(t))

	var props string
	require.NoError(t, db.QueryRow(`SELECT props FROM nodes WHERE id='pkg'`).Scan(&props))
	assert.Contains(t, props, `"lang":"go"`)
	assert.NotContains(t, props, "Z28=",
		`base64 of "go" indicates a revert to map[string][]byte`)

	var round map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(props), &round))
	assert.JSONEq(t, `{"fmt":"fmt"}`, string(round["imports"]))
}
