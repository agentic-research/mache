package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removedSymbols are identifiers mache has deleted. Comments may still mention
// them — history is often the clearest way to explain why current code looks
// the way it does, and ast_walker.go's parity notes are genuinely useful. What
// must not happen is a NEW comment describing one as a live code path.
//
// This ratchet exists because cmd/serve.go told readers the auto-leyline
// fallback used "the in-process Engine + SitterWalker path" for eleven minors
// after SitterWalker was deleted (mache-6dda39). Nothing caught it: a comment
// cannot fail to compile.
var removedSymbols = map[string]string{
	"SitterWalker":   "in-process tree-sitter walker, removed in v0.18.0 (ADR-0012 step 4)",
	"sitter_flatten": "tree-sitter AST flattener, removed in v0.18.0",
	"cgofuse":        "FUSE backend, removed in v0.7.0 (ADR-0006)",
}

// removedSymbolBaseline freezes how many times each file mentions a removed
// symbol today. New mentions fail; the counts may only shrink (see
// TestRemovedSymbolBaselineHasNoStaleEntries).
//
// Every entry here is a comment about history, not a claim about live code. If
// you are adding to this map, that is the thing to double-check.
var removedSymbolBaseline = map[string]int{
	"internal/leylinegraph/call_extractor_ast.go": 1,
	"cmd/serve.go":                           1,
	"graph/composite.go":                     1,
	"internal/ingest/ast_walker.go":          7,
	"internal/ingest/ast_walker_calls.go":    3,
	"internal/ingest/ast_walker_extract.go":  3,
	"internal/ingest/ast_walker_index.go":    1,
	"internal/ingest/ast_walker_selector.go": 3,
	"internal/ingest/engine_languages.go":    2,
	"internal/ingest/engine_walk.go":         1,
}

func scanRemovedSymbols(t *testing.T) map[string]int {
	t.Helper()
	root := moduleRoot(t)
	counts := map[string]int{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "target", "testdata", "_probe":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for line := range strings.SplitSeq(string(data), "\n") {
			for sym := range removedSymbols {
				if strings.Contains(line, sym) {
					counts[rel]++
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	return counts
}

// TestNoNewReferencesToRemovedSymbols is the ratchet. A file gaining a mention
// of a deleted symbol fails here, which is what the serve.go regression needed.
func TestNoNewReferencesToRemovedSymbols(t *testing.T) {
	counts := scanRemovedSymbols(t)

	var problems []string
	for file, n := range counts {
		allowed, ok := removedSymbolBaseline[file]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"  %s: %d mention(s) of a removed symbol, not in the baseline", file, n))
		case n > allowed:
			problems = append(problems, fmt.Sprintf(
				"  %s: %d mention(s), baseline allows %d", file, n, allowed))
		}
	}
	sort.Strings(problems)

	if len(problems) > 0 {
		var syms []string
		for s, why := range removedSymbols {
			syms = append(syms, "    "+s+" — "+why)
		}
		sort.Strings(syms)
		assert.Fail(t, "new reference(s) to a symbol mache has deleted",
			"%s\n\nRemoved symbols:\n%s\n\nA comment may describe one as history "+
				"(\"matching SitterWalker's per-scope behavior\") but must not describe it as a "+
				"live code path. If this mention is genuinely historical, raise the count in "+
				"removedSymbolBaseline.", strings.Join(problems, "\n"), strings.Join(syms, "\n"))
	}
}

// TestRemovedSymbolBaselineHasNoStaleEntries makes the ratchet one-directional:
// once a mention is cleaned up, its slot cannot be silently reused.
func TestRemovedSymbolBaselineHasNoStaleEntries(t *testing.T) {
	counts := scanRemovedSymbols(t)

	for file, allowed := range removedSymbolBaseline {
		actual := counts[file]
		assert.LessOrEqual(t, allowed, actual,
			"baseline for %s allows %d mention(s) but only %d remain — lower it to %d "+
				"so the ratchet keeps tightening", file, allowed, actual, actual)
	}
}

// TestRemovedSymbolsAreActuallyRemoved keeps the list honest in the other
// direction: if a symbol comes back, its entry must go, or the ratchet starts
// policing live code.
func TestRemovedSymbolsAreActuallyRemoved(t *testing.T) {
	root := moduleRoot(t)

	for sym := range removedSymbols {
		found := false
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.Contains(path, "/.claude/") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			for _, decl := range []string{"type " + sym + " ", "func " + sym + "(", ") " + sym + "("} {
				if strings.Contains(string(data), decl) {
					found = true
				}
			}
			return nil
		})
		require.NoError(t, err)
		assert.False(t, found,
			"%q is listed as removed but is declared somewhere — drop it from removedSymbols", sym)
	}
}
