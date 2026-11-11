package lloschema_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/agentic-research/mache/internal/lltest"
)

// TestParserID_ReadsTheDerivation, on real parses: the parserId is the
// derived_from field leyline stamps, the same for every tree one parser
// parses and different under another extraction epoch (mache-3465b9).
func TestParserID_ReadsTheDerivation(t *testing.T) {
	parse := func(t *testing.T, body string) string {
		src := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"), []byte(body), 0o644))
		db, _ := lltest.ParseSourceViaLeyline(t, src)
		id, err := lloschema.ParserID(lloschema.DB(db))
		require.NoError(t, err)
		assert.Len(t, id, 64, "a 32-byte BLAKE3 hash in hex")
		return id
	}
	a := parse(t, "package a\n")
	assert.Equal(t, a, parse(t, "package b\n\nfunc B() {}\n"), "the parser, not the tree")
	t.Setenv("LLO_EXTRACTION_EPOCH", "6")
	assert.NotEqual(t, a, parse(t, "package a\n"), "another extraction epoch is another parser")
}

// TestParserID_RefusesADatabaseThatRecordsNone: a database with no derived_from
// v2 has no parser identity to key on.
func TestParserID_RefusesADatabaseThatRecordsNone(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = lloschema.ParserID(lloschema.DB(db))
	assert.ErrorIs(t, err, lloschema.ErrNoParserID, "no _meta at all")
}
