package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

// fixtureShapedTables is the table set the TEST-file half of this rule guards.
// It is lloOwnedTables PLUS node_defs / node_refs.
//
// Those two are absent from lloOwnedTables because mache legitimately WRITES
// them in production (internal/ingest.SQLiteWriter) — that is the contract. But
// a TEST that hand-builds one is a different defect: node_refs is precisely the
// table whose column set decides which arm of ensureCanonicalViews runs, and
// whose node_id column means the ENCLOSING CONSTRUCT on the mache projection
// and the CALL-SITE LEAF on ley-line. A fixture picks one of those meanings by
// what it INSERTs, with nothing in the test saying which. So for tests the
// question is not "who owns the table" but "did this fixture invent a shape",
// and the answer must be no (mache-7555da).
var fixtureShapedTables = append(append([]string{}, lloOwnedTables...), "node_defs", "node_refs")

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

	// THE ONE SANCTIONED WRITER, and the reason the test-file half of this rule
	// can exist at all (mache-7555da).
	//
	// internal/fixturedb is where every test fixture's LLO-shaped DDL now
	// lives. It is a violation of the same layering — but a SINGLE one, whose
	// statements are DERIVED from the pinned producer's own sqlite_master and
	// re-derived by a conformance test that fails on drift. That is strictly
	// stronger than the 34 hand-written spellings it replaced, none of which
	// matched ley-line at all. When delegation lands these disappear with the
	// rest.
	"internal/fixturedb/schema_leyline.go": "the DERIVED ley-line DDL, conformance-tested against the pinned binary (mache-7555da)",
	"internal/fixturedb/build.go":          "creates the derived tables for a fixture (mache-7555da)",
	"internal/fixturedb/emit.go":           "inserts fixture rows into the derived tables (mache-7555da)",
}

// lloTestFixtureAllowlist is the frozen set of TEST files still hand-building
// LLO-shaped tables, as repo-relative paths. It is a RATCHET: it may only
// SHRINK, and the way to shrink it is to build the fixture with
// internal/fixturedb instead.
//
// It is a SEPARATE list from lloWriterAllowlist on purpose. Folding thirty
// fixture files into that map would bury the handful of production entries that
// actually describe the boundary, and a rule whose signal is buried stops being
// read. These entries share one reason, so they are a list rather than a map.
//
// Why they are a violation at all, when they are "just tests": each states its
// OWN idea of ley-line's schema, and ensureCanonicalViews emits a structurally
// different v_refs / v_defs body per column combination. So the DDL literal in
// a fixture is a hidden parameter selecting which SQL the rule under test
// executes — a rule tested against a two-column node_refs runs the degraded arm
// while production runs the full arm, and neither side notices. See
// internal/fixturedb's package doc for the measurement (mache-7555da).
var lloTestFixtureAllowlist = []string{
	"cmd/build_leyline_coverage_test.go",
	"cmd/cache_ast_test.go",
	"cmd/cache_test.go",
	"cmd/call_extractor_ast_test.go",
	"cmd/dead_code_skip_list_retreat_test.go",
	"cmd/duplicate_definitions_rust_test.go",
	"cmd/falsifiability_lsp_projection_test.go",
	"cmd/falsifiability_skip_list_ablation_test.go",
	"cmd/find_smells_cli_test.go",
	"cmd/find_smells_duplicate_code_test.go",
	"cmd/kind_discriminator_leyline_ast_test.go",
	"cmd/leyline_callees_test.go",
	"cmd/leyline_test.go",
	"cmd/serve_find_smells_bench_test.go",
	"cmd/serve_find_smells_test.go",
	"cmd/serve_find_smells_views_test.go",
	"cmd/serve_lsp_step5_test.go",
	"cmd/smell_findings_incremental_test.go",
	"internal/graph/graph_suite_test.go",
	"internal/graph/props_sql_test.go",
	"internal/graph/sqlite_graph_lookupdef_test.go",
	"internal/graph/sqlite_graph_nodehash_test.go",
	"internal/ingest/ast_flatten_db_test.go",
	"internal/ingest/ast_walker_bench_test.go",
	"internal/ingest/ast_walker_callees_e2e_test.go",
	"internal/ingest/ast_walker_calls_test.go",
	"internal/ingest/ast_walker_core_test.go",
	"internal/ingest/ast_walker_test.go",
	"internal/ingest/inner_scope_test.go",
	"internal/ingest/invalidate_test.go",
	"internal/ingest/sqlite_writer_test.go",
}

// TestLLOBoundary_NoNewWritersOfLLOOwnedTables fails when a file not on the
// relevant allowlist writes a table LLO owns.
//
// Covers PRODUCTION and TEST files, against two separate allowlists. Test files
// used to be skipped outright, with the note that fixture drift was "a real
// problem but a DIFFERENT one". internal/fixturedb closed that problem
// (mache-7555da), so the skip became the thing holding a 35th hand-built
// fixture's door open — and this is the widening that was promised there.
func TestLLOBoundary_NoNewWritersOfLLOOwnedTables(t *testing.T) {
	root := repoRootForBoundary(t)

	testAllowed := map[string]bool{}
	for _, rel := range lloTestFixtureAllowlist {
		testAllowed[rel] = true
	}

	type violation struct{ file, table, verb string }
	var found, foundTests []violation

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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		isTest := strings.HasSuffix(rel, "_test.go")
		if isTest {
			if testAllowed[rel] {
				return nil
			}
		} else if _, allowed := lloWriterAllowlist[rel]; allowed {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Unparseable Go is a different test's problem.
			return nil //nolint:nilerr // not this invariant's concern
		}

		// Test files are held to the WIDER table set: a fixture that invents a
		// node_defs / node_refs shape is the hidden-parameter defect even
		// though production legitimately writes those tables.
		guarded := lloOwnedTables
		if isTest {
			guarded = fixtureShapedTables
		}

		// Walk string literals rather than matching the file as text: SQL in
		// this repo lives in string literals, and reading the AST avoids
		// importing regexp (see regexpAllowlist in this package).
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			upper := sqlText(lit)
			for _, verb := range sqlWriteVerbs {
				if !strings.Contains(upper, verb) {
					continue
				}
				for _, tbl := range guarded {
					if !writesTable(upper, verb, tbl) {
						continue
					}
					v := violation{rel, tbl, strings.TrimSpace(verb)}
					if isTest {
						foundTests = append(foundTests, v)
					} else {
						found = append(found, v)
					}
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)

	if len(found) > 0 {
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
		t.Error(b.String())
	}

	if len(foundTests) > 0 {
		sort.Slice(foundTests, func(i, j int) bool { return foundTests[i].file < foundTests[j].file })
		var b strings.Builder
		b.WriteString("test files outside lloTestFixtureAllowlist hand-build LLO-owned tables:\n")
		for _, v := range foundTests {
			b.WriteString("  " + v.file + ": " + v.verb + " " + v.table + "\n")
		}
		b.WriteString("\nBuild the fixture with internal/fixturedb instead. A hand-written\n")
		b.WriteString("CREATE TABLE is a HIDDEN TEST PARAMETER: ensureCanonicalViews probes for\n")
		b.WriteString("columns and emits a structurally different v_refs/v_defs per combination,\n")
		b.WriteString("so the DDL a fixture happens to type decides which SQL the rule under\n")
		b.WriteString("test actually runs. fixturedb.New(t, fixturedb.Leyline|Standalone) makes\n")
		b.WriteString("that choice explicit and derives the shape from the real producer.\n")
		b.WriteString("The allowlist may only SHRINK (mache-7555da).\n")
		t.Error(b.String())
	}
}

// sqlText returns a string literal's VALUE — escapes resolved, quotes gone —
// uppercased for matching.
//
// Unquoting is load-bearing, not tidiness. This used to match against
// lit.Value, the raw source text, where a newline inside an interpreted string
// is the two characters `\` `n`. The whitespace cutset therefore had to include
// a backslash and an `n` — and since the text was uppercased first, an `N`. That
// cutset then ate the leading `N` of any table name starting with one, so
// `nodes`, `node_content`, `node_defs` and `node_refs` could NEVER match. One of
// those, node_content, is in lloOwnedTables: the rule has been silently unable
// to enforce it since it was written (found while widening this rule for
// mache-7555da).
func sqlText(lit *ast.BasicLit) string {
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		// Not a form strconv understands; fall back to the raw text rather than
		// dropping the literal from the scan entirely.
		return strings.ToUpper(lit.Value)
	}
	return strings.ToUpper(unquoted)
}

// writesTable reports whether an uppercased SQL string (see [sqlText]) writes
// tbl with verb. Matches the table name as a whole token so `_ast` does not
// match `_ast_pointer` and `_lsp` does not match `_lsp_defs` — those are
// separate entries and should be reported separately.
func writesTable(upperSQL, verb, tbl string) bool {
	target := strings.ToUpper(tbl)
	idx := strings.Index(upperSQL, verb)
	for idx >= 0 {
		rest := strings.TrimLeft(upperSQL[idx+len(verb):], " \t\n\r")
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
	rels := make([]string, 0, len(lloWriterAllowlist)+len(lloTestFixtureAllowlist))
	for rel := range lloWriterAllowlist {
		rels = append(rels, rel)
	}
	rels = append(rels, lloTestFixtureAllowlist...)
	for _, rel := range rels {
		_, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, parser.PackageClauseOnly)
		require.NoError(t, err,
			"allowlisted file %s no longer parses or does not exist — remove the entry "+
				"rather than leaving a stale exemption", rel)
	}
}

// TestLLOBoundary_TestFixtureAllowlistIsStillEarned is the shrink half of the
// ratchet: an entry for a file that no longer hand-builds an LLO table is a
// stale exemption, and leaving it lets the next hand-built fixture back in
// under cover of a migration that already happened.
func TestLLOBoundary_TestFixtureAllowlistIsStillEarned(t *testing.T) {
	root := repoRootForBoundary(t)
	for _, rel := range lloTestFixtureAllowlist {
		path := filepath.Join(root, rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err, "parse %s", rel)

		writes := false
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			upper := sqlText(lit)
			for _, verb := range sqlWriteVerbs {
				for _, tbl := range fixtureShapedTables {
					if writesTable(upper, verb, tbl) {
						writes = true
						return false
					}
				}
			}
			return true
		})
		assert.True(t, writes,
			"%s no longer writes an LLO-owned table — it was migrated to "+
				"internal/fixturedb, so DELETE its lloTestFixtureAllowlist entry "+
				"rather than leaving the exemption behind", rel)
	}
}
