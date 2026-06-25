package ingest

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/agentic-research/mache/internal/lang"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileLevelParitySrc exercises every file-level ref pattern:
//   - a cobra-style struct var whose field VALUE is a func reference
//     (keyed_element > literal_element > identifier)
//   - a bare call            (call_expression > identifier)
//   - a selector call        (call_expression > selector_expression > field_identifier)
//   - an identifier argument (call_expression > argument_list > identifier)
//
// These are the top-level/nested identifiers that per-scope ExtractCalls
// can't see and that feed the _file_level: sentinel dead_code reads.
const fileLevelParitySrc = `package demo

import "fmt"

func handler() {}

var rootCmd = Command{
	Use:  "root",
	RunE: handler,
}

func run() {
	fmt.Println(greet())
	handler()
}

func greet() string { return "hi" }

type Command struct {
	Use  string
	RunE func()
}
`

// TestASTFileLevelRefsParity_Go asserts ASTWalker.ExtractFileLevelRefs
// (pure-Go SQL over _ast/nodes) returns the SAME token set as
// SitterWalker.ExtractFileLevelRefs (CGO tree-sitter). This is the
// missing file-level extractor that S4 needs before it can stop running
// the CGO parse in ingest — the projection parity gate (callers-based)
// excludes the _file_level: sentinel, so this divergence would otherwise
// be invisible.
func TestASTFileLevelRefsParity_Go(t *testing.T) {
	src := []byte(fileLevelParitySrc)
	grammar := lang.ForName("go").Grammar()

	// SitterWalker (CGO) — parse live, extract from the file root.
	parser := sitter.NewParser()
	parser.SetLanguage(grammar)
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	require.NoError(t, err)
	defer tree.Close()
	sw := NewSitterWalker()
	defer sw.Close()
	sitterRefs, err := sw.ExtractFileLevelRefs(tree.RootNode(), src, grammar, "go")
	require.NoError(t, err)
	require.NotEmpty(t, sitterRefs, "fixture must exercise file-level refs (non-vacuity guard)")

	// ASTWalker (pure-Go) — query an _ast db emitted from the SAME parse.
	db := sitterToASTDB(t, filepath.Join(t.TempDir(), "ast.db"), "demo.go", "go", src)
	defer func() { _ = db.Close() }()
	astRefs, err := NewASTWalker(db).ExtractFileLevelRefs("demo.go", "go")
	require.NoError(t, err)

	assert.Equal(t, sortedCopy(sitterRefs), sortedCopy(astRefs),
		"file-level ref token set must match across walkers")
}

// TestASTContextParity_Go asserts ASTWalker.ExtractContext matches
// SitterWalker.ExtractContext — the top-level import/const/var/type
// source blob the engine attaches to context fields. Also a building
// block S4 serves from SQL instead of the CGO parse.
func TestASTContextParity_Go(t *testing.T) {
	src := []byte(fileLevelParitySrc)
	grammar := lang.ForName("go").Grammar()

	parser := sitter.NewParser()
	parser.SetLanguage(grammar)
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	require.NoError(t, err)
	defer tree.Close()
	sw := NewSitterWalker()
	defer sw.Close()
	sitterCtx, err := sw.ExtractContext(tree.RootNode(), src, grammar, "go")
	require.NoError(t, err)
	require.NotEmpty(t, sitterCtx, "fixture must produce a context blob (non-vacuity guard)")

	db := sitterToASTDB(t, filepath.Join(t.TempDir(), "ast.db"), "demo.go", "go", src)
	defer func() { _ = db.Close() }()
	astCtx, err := NewASTWalker(db).ExtractContext("demo.go", "go")
	require.NoError(t, err)

	assert.Equal(t, string(sitterCtx), string(astCtx),
		"context blob must match across walkers")
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
