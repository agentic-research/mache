package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The LLO boundary, enforced.
//
// ley-line-open-2037b4 settled the ecosystem split as a ROLE assignment, not a
// performance argument:
//
//	data plane     moves and stores bytes: content addressing, the arena, CAS,
//	               projections, mount, CDC                              -> LLO
//	control plane  decides what to do with them: smell rules, queries,
//	               code intelligence, the MCP surface                   -> mache
//
// Under that split, mache writing LLO's schema is a control-plane component
// doing data-plane work — a layering violation whether or not it is fast.
//
// LLO's own framing of how to keep it honest: the falsifiable question is not
// "is delegation faster" but "does the BOUNDARY HOLD — does any data-plane
// logic remain on the mache side?", and that it is "checkable by inspection and
// by a RULE, in a way a benchmark is not". The precedent cited is
// cloister-5e4402 ("anything in cloister named `leyline-*` is a smell;
// primitives belong in LLO"). This is mache's half of that.
//
// It is deliberately a RATCHET rather than a ban, following regexpAllowlist in
// this package. mache writes some of these tables today; delegation is not
// shipped. A rule that failed on every existing violation would be turned off,
// and a rule that is off enforces nothing. This one freezes the current set so
// the boundary can only tighten — and makes each remaining violation carry a
// written reason instead of being invisible.
//
// Without it, "mache is the control plane" is folklore: true when stated,
// unenforced afterwards, and quietly false a few PRs later.

// lloOwnedTables are the tables whose SCHEMA AND CONTENTS ley-line-open owns.
//
// Per ley-line-open-040775, `nodes` is the cross-runtime AUTHORITATIVE schema
// that consumers may write; `content_chunks` / `content_manifest` /
// `content_manifest_meta` are LLO-owned derived indexes and "no consumer writes
// LLO-owned content_* tables directly". The `_`-prefixed producer tables are
// LLO's parse output: mache reads them, LLO produces them.
//
// `nodes` is deliberately ABSENT — mache legitimately writes it, and that is
// the contract.
var lloOwnedTables = []string{
	"_ast",
	"_ast_pointer",
	"_source",
	"_lsp",
	"_lsp_defs",
	"_lsp_refs",
	"_lsp_hover",
	"node_content",
	"content_chunks",
	"content_manifest",
	"content_manifest_meta",
}

// sqlWriteVerbs are the statement kinds that constitute WRITING. Reading LLO's
// output is the entire point of the consumer relationship and is never a
// violation.
var sqlWriteVerbs = []string{
	"INSERT INTO",
	"INSERT OR REPLACE INTO",
	"INSERT OR IGNORE INTO",
	"UPDATE ",
	"DELETE FROM",
	"CREATE TABLE",
	"DROP TABLE",
	"ALTER TABLE",
}

// lloWriterAllowlist is the frozen set of files permitted to write LLO-owned
// tables, as repo-relative paths. It is a RATCHET: the set may only SHRINK.
//
// Each entry is a boundary violation with a reason, not a blessing. When
// delegation lands (mache-e64f36) these should disappear rather than be
// re-justified.
var lloWriterAllowlist = map[string]string{
	// PRODUCTION VIOLATIONS — real, and the reason this test is a ratchet.
	//
	// cmd/cache.go materialises a pulled cache entry by CREATEing and INSERTing
	// _source and _ast directly, i.e. mache writes LLO's parse output rather
	// than obtaining it from LLO. Under the role split this is the clearest
	// data-plane work left in mache, and it is exactly what delegating to the
	// arena removes. Tracked by mache-e64f36; do not re-justify, delete.
	"cmd/cache.go": "materialises _source/_ast when hydrating a pulled cache entry — data-plane work pending delegation (mache-e64f36)",

	// TOOLING — generates .db fixtures shaped like LLO output so tests can run
	// without a leyline binary. Defensible today (fixtures must exist before
	// the thing that produces them is a dependency) but it does encode LLO's
	// schema in mache, so it drifts silently when LLO changes shape. Better end
	// state: generate fixtures BY RUNNING leyline, as the leyline_ast test
	// helper already does.
	"tools/gen-lsp-fixture/main.go": "generates LSP-shaped .db fixtures; should generate via leyline instead",

	// Builds an IN-MEMORY _ast with a REDUCED shape (node_id, node_kind,
	// start_row only) as scratch for lint queries. It does not persist LLO
	// data, so it is the mildest of the three — but it hardcodes LLO's table
	// name and a subset of its columns, so it drifts silently when LLO changes
	// shape, with no version or lineage marker to notice by.
	"internal/linter/linter.go": "synthesises a reduced in-memory _ast for lint queries — encodes LLO's schema shape",
}

// TestLLOBoundary_NoNewWritersOfLLOOwnedTables fails when a file not on the
// allowlist writes a table LLO owns.
//
// Scoped to PRODUCTION files. Test files are excluded, and that is a scope
// judgement rather than an oversight: 24 of them hand-build LLO-shaped tables
// as fixtures, which is a real problem (a fixture asserts against mache's IDEA
// of LLO's schema, so the two drift without either side noticing) but a
// DIFFERENT one from a layering violation. Allowlisting 24 files here would
// bury the three production entries that actually describe the boundary, and a
// rule whose signal is buried stops being read. Fixture drift is tracked
// separately as mache-7555da; when it is fixed, widening this rule to tests is
// a one-line change.
func TestLLOBoundary_NoNewWritersOfLLOOwnedTables(t *testing.T) {
	root := repoRootForBoundary(t)

	type violation struct{ file, table, verb string }
	var found []violation

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", ".claude", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, allowed := lloWriterAllowlist[rel]; allowed {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Unparseable Go is a different test's problem.
			return nil //nolint:nilerr // not this invariant's concern
		}

		// Walk string literals rather than matching the file as text: SQL in
		// this repo lives in string literals, and reading the AST avoids
		// importing regexp (see regexpAllowlist in this package).
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			upper := strings.ToUpper(lit.Value)
			for _, verb := range sqlWriteVerbs {
				if !strings.Contains(upper, verb) {
					continue
				}
				for _, tbl := range lloOwnedTables {
					if writesTable(upper, verb, tbl) {
						found = append(found, violation{rel, tbl, strings.TrimSpace(verb)})
					}
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)

	if len(found) == 0 {
		return
	}
	sort.Slice(found, func(i, j int) bool { return found[i].file < found[j].file })
	var b strings.Builder
	b.WriteString("files outside the allowlist write LLO-owned tables:\n")
	for _, v := range found {
		b.WriteString("  " + v.file + ": " + v.verb + " " + v.table + "\n")
	}
	b.WriteString("\nmache is the CONTROL plane; LLO owns the data plane (ley-line-open-2037b4).\n")
	b.WriteString("Writing LLO's schema from mache is a layering violation. Obtain the data\n")
	b.WriteString("from LLO, or — if this is genuinely unavoidable — add the file to\n")
	b.WriteString("lloWriterAllowlist with a one-line reason. The allowlist may only SHRINK.\n")
	t.Fatal(b.String())
}

// writesTable reports whether an uppercased SQL literal writes tbl with verb.
// Matches the table name as a whole token so `_ast` does not match
// `_ast_pointer` and `_lsp` does not match `_lsp_defs` — those are separate
// entries and should be reported separately.
func writesTable(upperSQL, verb, tbl string) bool {
	target := strings.ToUpper(tbl)
	idx := strings.Index(upperSQL, verb)
	for idx >= 0 {
		rest := strings.TrimLeft(upperSQL[idx+len(verb):], " \t\\N\"`")
		// CREATE TABLE may carry IF NOT EXISTS before the name.
		rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
		rest = strings.TrimLeft(rest, " \t")
		if strings.HasPrefix(rest, target) {
			after := rest[len(target):]
			if after == "" || !isIdentRune(after[0]) {
				return true
			}
		}
		next := strings.Index(upperSQL[idx+1:], verb)
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	return false
}

func isIdentRune(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func repoRootForBoundary(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// TestLLOBoundary_AllowlistEntriesStillExist keeps the ratchet honest in the
// other direction: an allowlist entry for a deleted or renamed file is a stale
// exemption that would silently re-permit a violation if the path came back.
func TestLLOBoundary_AllowlistEntriesStillExist(t *testing.T) {
	root := repoRootForBoundary(t)
	for rel := range lloWriterAllowlist {
		_, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, parser.PackageClauseOnly)
		require.NoError(t, err,
			"allowlisted file %s no longer parses or does not exist — remove the entry "+
				"rather than leaving a stale exemption", rel)
	}
}
