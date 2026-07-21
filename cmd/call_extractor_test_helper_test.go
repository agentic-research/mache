package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/leyline"

	_ "modernc.org/sqlite"
)

// attachLeylineASTWalkerForTest is the test-only sibling of the production
// attachLeylineASTWalker: it resolves the PINNED leyline WITHOUT downloading
// (ResolveBinary(false)) so offline CI skips instead of hanging, parses
// dataSource into an `_ast` db, and wires an ASTWalker onto the engine.
// Returns the open db, a cleanup, and an error (which callers turn into a Skip).
func attachLeylineASTWalkerForTest(t *testing.T, dataSource string, engine *ingest.Engine) (*sql.DB, func(), error) {
	t.Helper()
	bin, err := leyline.ResolveBinary(false)
	if err != nil {
		return nil, nil, err
	}
	dbPath := filepath.Join(t.TempDir(), "ast.db")
	out, err := exec.Command(bin, "parse", dataSource, "-o", dbPath).CombinedOutput() //nolint:gosec // test-only, pinned binary
	if err != nil {
		return nil, nil, fmt.Errorf("leyline parse: %w: %s", err, string(out))
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open ley-line parse output: %w", err)
	}
	engine.SetASTWalker(ingest.NewASTWalker(db))
	cleanup := func() {
		_ = db.Close()
		_ = os.Remove(dbPath)
	}
	return db, cleanup, nil
}

// testGoCallExtractor is a naive, pure-Go call extractor for tests that build
// synthetic MemoryStores from raw Go source bytes (node Data). ADR-0012 step 4
// removed the in-process CGO tree-sitter extractor (newCallExtractor), and the
// production AST extractor (newASTCallExtractor) reads a ley-line `_ast` db by
// source_id — it cannot parse ad-hoc Data byte slices. These composite/mount
// annotation tests only need *some* identifier-call to flow through the callees
// machinery, so a regex over `identifier(` suffices and keeps them CGO-free.
//
// It is deliberately not exported and not used in production wiring.
func testGoCallExtractor() graph.CallExtractor {
	callRe := regexp.MustCompile(`\b([A-Za-z_]\w*)\s*\(`)
	// Go keywords that can precede '(' but are not calls.
	keywords := map[string]bool{
		"func": true, "for": true, "if": true, "switch": true,
		"return": true, "go": true, "defer": true, "select": true,
	}
	return func(content []byte, _, langName string) ([]graph.QualifiedCall, error) {
		if langName != "go" && langName != "" {
			return nil, nil
		}
		seen := map[string]bool{}
		var calls []graph.QualifiedCall
		for _, m := range callRe.FindAllSubmatch(content, -1) {
			tok := string(m[1])
			if keywords[tok] || seen[tok] {
				continue
			}
			seen[tok] = true
			calls = append(calls, graph.QualifiedCall{Token: tok})
		}
		return calls, nil
	}
}
