package ingest

import (
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/require"
)

// The whole-file extraction caches (fileCallTokens / fileAddrRefs) must NOT
// store an empty partial result on a DB failure — they must surface the error.
// Otherwise a transient DB error on a long-lived serve would permanently empty
// a file's callees/refs with zero signal (mache-015f5c). "go" call patterns and
// address-ref queries are registered in engine_languages' init, so a closed DB
// drives the real query path to failure.

func TestASTWalkerExtractors_SurfaceDBError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extract func(*ASTWalker) error
		why     string
	}{
		{"ExtractCallsScoped", func(w *ASTWalker) error {
			_, err := w.ExtractCallsScoped("main.go", "main.go/scope", "go")
			return err
		}, "a DB failure must surface, not be cached as an empty callee set"},
		{"ExtractAddressRefs", func(w *ASTWalker) error {
			_, err := w.ExtractAddressRefs("main.go", "go")
			return err
		}, "a DB failure must surface, not be cached as an empty ref set"},
		// The find_callees live extractor must also surface DB errors, not
		// return a short list with nil error (mache-6ff371, completing
		// mache-015f5c).
		{"ExtractQualifiedCallsScoped", func(w *ASTWalker) error {
			_, err := w.ExtractQualifiedCallsScoped("main.go", "main.go/scope", "go")
			return err
		}, "a DB failure in the find_callees path must surface, not silently drop calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedTestAST(t, fixturedb.Leyline)
			w := NewASTWalker(db)
			require.NoError(t, db.Close()) // subsequent queries fail
			require.Error(t, tc.extract(w), tc.why)
		})
	}
}
