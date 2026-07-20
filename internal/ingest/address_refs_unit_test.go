package ingest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestExtractAddressRefs_NoRegistry — languages with no registered address ref
// queries return nil, without touching the database (the registry check
// short-circuits before any SQL). Pure Go, no leyline needed.
func TestExtractAddressRefs_NoRegistry(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	w := NewASTWalker(db)
	refs, err := w.ExtractAddressRefs("file.xyz", "nonexistent_lang")
	require.NoError(t, err)
	assert.Nil(t, refs, "should return nil for unregistered language")
}

func TestUnquoteCapture(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`"DATABASE_URL"`, "DATABASE_URL"},
		{`"hello world"`, "hello world"},
		{`""`, ""},
		{`bare_token`, "bare_token"},
		{`"with\"escape"`, `with"escape`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, unquoteCapture(tt.input))
		})
	}
}
