package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/lsp"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// SmellRule describes one structural code-smell pattern. Rules are
// language-aware: each carries the language(s) it applies to plus a
// SQL query template that runs against the `_ast` / `nodes` /
// `node_defs` / `node_refs` tables produced by `leyline parse`.
//
// The query MUST select exactly seven columns:
//
//	source_id, node_id, start_byte, end_byte, start_row, start_col, metric
//
// in that order. `metric` is an integer score (cyclomatic complexity,
// fan-out count, line length, etc.); binary rules that don't carry
// a metric emit `0 AS metric`. The handler shapes the row into a
// uniform response with 1-based line/column and an optional snippet.
//
// `ScopeColumn` is the SQL expression the handler will compare to
// source_id when the caller passes `source_id` to find_smells (e.g.
// "lit.source_id" for AST-walking rules; "n.source_file" for rules
// that join via nodes). The query template MUST contain a single
// `%s` placeholder where the scope clause should be spliced in;
// rules unconcerned with scoping can leave ScopeColumn blank.
//
// Threshold filtering on the metric column is server-side via the
// `min_metric` request arg — handler drops findings whose metric
// is below the cutoff before returning. Default 0 keeps every row.
//
// Bead mache-6z2e tracks the broader "machelint" idea — declarative
// rules consumed via this tool. Today the registry is hard-coded; in
// the future it becomes user-extensible (declarative JSON, per-repo
// overrides, etc.).
type SmellRule struct {
	ID          string   // stable identifier, used as the MCP tool argument
	Languages   []string // matches `_source.language` values; empty = any
	Description string   // shown in the help payload
	Query       string   // SQL with one `%s` placeholder for the optional scope clause
	ScopeColumn string   // SQL expression to compare to source_id; empty disables source_id scoping
	// Requires lists the SQL tables this rule reads. Surfaced in the
	// rules listing so an agent can decide upfront whether the rule is
	// usable on the active backend. Standalone mache emits `nodes`,
	// `node_refs`, `node_defs`; LLO-built .db additionally has `_ast`,
	// `_source`, `_imports`, `_lsp*`. See docs/ARCHITECTURE.md
	// "Interplay with ley-line-open" for the full table.
	Requires []string
	// DefaultMinMetric is the rule's recommended metric threshold
	// when the request omits min_metric. Zero means "no default
	// threshold; return all rows." Used for metric-bearing rules
	// where the query's natural output includes a long tail of
	// uninteresting low-metric rows (e.g. long_function returns
	// every function regardless of size; without a default
	// threshold the response is dominated by 1-line setters).
	// Callers can pass min_metric=0 explicitly to override the
	// default and see everything; any non-zero min_metric also
	// overrides.
	DefaultMinMetric int64
}

// smellRegistry holds the registered rules. Built-ins below; external
// rules from $MACHE_SMELL_RULES_DIR are appended in init() so they
// participate in discovery, listing, and dispatch the same way.
var smellRegistry = []SmellRule{
	{
		ID:          "magic_int_in_comparison",
		Languages:   []string{"go"},
		Description: "Go binary expressions where an int_literal appears as a direct operand. Each match is a candidate magic constant — replace with a named const if the value carries domain meaning.",
		Requires:    []string{"_ast", "nodes"},
		ScopeColumn: "lit.source_id",
		Query: `
			SELECT lit.source_id, lit.node_id, lit.start_byte, lit.end_byte, lit.start_row, lit.start_col, 0 AS metric
			FROM _ast lit
			JOIN nodes n   ON n.id = lit.node_id
			JOIN nodes p   ON p.id = n.parent_id
			JOIN _ast pa   ON pa.node_id = p.id
			WHERE lit.node_kind  = 'int_literal'
			  AND pa.node_kind   = 'binary_expression'
			%s
			ORDER BY lit.source_id, lit.start_byte
		`,
	},
	{
		ID:          "dead_code",
		Description: "Constructs whose tokens (any alias — bare 'Foo' or qualified 'pkg.Foo') have NO entries in node_refs. Aggregated per construct, not per token: a function with three token aliases where any one is referenced is treated as live. Skip list rejects entry points (main, init), interface methods invoked dynamically (String, Error, Read, Write, ...), and the testing-framework prefixes Test*/Benchmark*/Example*/Fuzz* (the runtime invokes those reflectively). Restricted to call-target-shaped categories ('functions/', 'methods/'): types, constants, variables, and imports are referenced as identifiers rather than called, so node_refs (call-extraction-only) doesn't reflect their use; flagging them is pure noise. source_id falls back to a child's source_file when the construct dir itself doesn't carry one (the schema engine sets source_file on leaf nodes — `source`, `ast.json`, `doc` — but not on the wrapping construct dir). Generated code files (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) are excluded because they intentionally export wide APIs the consumer picks from. False positives still expected for exported API consumed outside the indexed scope.",
		Requires:    []string{"node_defs", "node_refs", "nodes"},
		ScopeColumn: "COALESCE(NULLIF(n.source_file, ''), cs.source_file, '')",
		Query: `
			-- A construct is "alive" if ANY of its token aliases appears
			-- in node_refs. We flag a construct as dead only when every
			-- one of its tokens is unreferenced.
			--
			-- The JOIN also matches when ref.token equals the LEAF of
			-- def.token after the FIRST '.' — this handles methods
			-- rendered as 'Receiver.Method' (the go-schema methods
			-- branch). Call-extraction captures obj.Method() as just
			-- 'Method', so without the strip every method on a typed
			-- receiver would look dead. We strip on the first dot only
			-- (not the last) because that's the receiver boundary in
			-- 'Receiver.Method'; the package-qualified shape
			-- 'pkg.Receiver.Method' is also registered as the bare
			-- rendered alias 'Receiver.Method', whose first-dot strip
			-- gives 'Method' as expected. Doing it as a JOIN predicate
			-- rather than registering bare-leaf aliases at ingest time
			-- avoids inflating duplicate_definitions (every interface
			-- method like 'AddDef' or 'Read' would show up N copies,
			-- one per implementing type, even though the implementations
			-- are conceptually distinct).
			-- Per ADR-0013 Step 4: query v_defs / v_refs (the canonical
			-- views) rather than node_defs / node_refs directly. The
			-- alive-check has two arms:
			--   L_1 binding: r.target_node_id = d.node_id — exact match
			--                from an LSP-resolved reference. Only fires
			--                on rows where Step 1 (ley-line-453f7e) wrote
			--                referrer_node_id + ref_token to _lsp_refs.
			--   L_0 mention: token-textual fallback for tree-sitter rows
			--                (target_node_id IS NULL). Includes the
			--                first-dot strip for 'Receiver.Method' →
			--                'Method' that handles methods rendered as
			--                'Receiver.Method'.
			-- Today (pre-Step-1), v_refs only surfaces L_0 rows so the
			-- fallback always runs and the result is identical to the
			-- legacy node_refs query. Once binding rows arrive, the
			-- L_1 arm dominates and the skip-list below becomes
			-- redundant for the cases LSP actually sees.
			WITH alive AS (
				SELECT DISTINCT d.node_id
				FROM v_defs d
				JOIN v_refs r
				  ON r.target_node_id = d.node_id
				  OR (r.target_node_id IS NULL AND (
				       r.token = d.token
				       OR (instr(d.token, '.') > 0 AND r.token = substr(d.token, instr(d.token, '.') + 1))
				     ))
			),
			-- Skip-list: defs whose alive-status this rule shouldn't try
			-- to determine textually. Two categories, with different
			-- conditions on whether they apply:
			--
			-- 1. Runtime-invoked entry points and test prefixes:
			--      main, init, Test*, Benchmark*, Example*, Fuzz*
			--    The Go runtime (loader, test harness) calls these
			--    reflectively. Neither tree-sitter NOR LSP synthesizes
			--    a reference for those dispatches — nothing static can
			--    see them. Lattice ceiling — skipped unconditionally.
			--
			-- 2. Interface-contract method names:
			--      Read/Write/Close (io), String/Error (fmt),
			--      Len/Less/Swap (sort.Interface), MarshalJSON,
			--      ServeHTTP, the SQLite vtab interface, the
			--      billy.Filesystem interface, ...
			--    External runtimes / libraries dispatch to these via
			--    interface, which tree-sitter's call extractor can't
			--    see. But LSP can — gopls knows r.Read() on an
			--    io.Reader resolves to the implementing type's Read
			--    method. ADR-0013 Falsifiability A confirmed the
			--    alive-check binding arm (r.target_node_id =
			--    d.node_id) handles these cases when LSP coverage is
			--    present. So: skip a def with one of these tokens
			--    only if NO binding row (v_refs.fidelity='binding')
			--    points at that specific def. With LSP coverage, the
			--    alive-check decides; without LSP coverage, the
			--    skip-list compensates as before. Net: precise
			--    retreat — skip-list shrinks automatically as LSP
			--    coverage grows, and a genuinely dead method whose
			--    name happens to match the list can now be flagged
			--    when LSP saw the type but no references to that
			--    method.
			skipped AS (
				-- Category 1: always skipped (runtime / test harness).
				SELECT DISTINCT v_defs.node_id FROM v_defs
				WHERE v_defs.token IN ('main', 'init')
				   OR substr(v_defs.token, instr(v_defs.token, '.') + 1) LIKE 'Test%%'
				   OR substr(v_defs.token, instr(v_defs.token, '.') + 1) LIKE 'Benchmark%%'
				   OR substr(v_defs.token, instr(v_defs.token, '.') + 1) LIKE 'Example%%'
				   OR substr(v_defs.token, instr(v_defs.token, '.') + 1) LIKE 'Fuzz%%'

				UNION

				-- Category 2: interface contracts — skipped only when
				-- LSP did NOT index this def at all. The presence of
				-- a binding-fidelity v_defs row means LSP saw the
				-- def; absence means LSP either didn't run or
				-- couldn't resolve the source file. Three states:
				--   (a) No binding v_defs row → LSP didn't see this
				--       def. Tree-sitter compensation still load-
				--       bearing → SKIP.
				--   (b) Binding v_defs row exists, NO binding v_refs
				--       row targets it → LSP saw the def, found no
				--       references. CONFIRMED DEAD by LSP. Don't
				--       skip — let alive-check flag it.
				--   (c) Binding v_defs row exists, ≥1 binding v_refs
				--       row targets it → ALIVE via alive-check
				--       binding arm. Don't skip; alive-check makes
				--       the call via target_node_id match.
				-- The NOT EXISTS gate triggers (a) only.
				SELECT DISTINCT v_defs.node_id FROM v_defs
				WHERE v_defs.token IN (
					'String','Error','Read','Write','Close','Len','Less','Swap',
					'MarshalJSON','UnmarshalJSON','Format','Scan','GoString',
					'BestIndex','Column','Connect','Create','Destroy','Disconnect',
					'Eof','Filter','Rowid','Open','Next',
					'Chroot','Readlink','Sys','TempFile','MkdirAll','Symlink',
					'Lstat','Stat','ReadDir','Capabilities','Root',
					'ServeHTTP'
				)
				  AND NOT EXISTS (
					SELECT 1 FROM v_defs lsp_d
					WHERE lsp_d.node_id = v_defs.node_id
					  AND lsp_d.fidelity = 'binding'
				  )
			),
			-- Resolve source_file via children when the construct dir
			-- itself has none. The schema engine attaches Origin (and
			-- thus source_file) to the leaf rendered files (source,
			-- ast.json, doc) but not to the wrapping construct dir.
			-- Without this fallback every dead_code finding scopes to
			-- '' (the empty string) and the find_smells GHA filters
			-- nothing against the PR diff.
			child_source AS (
				SELECT parent_id AS node_id,
				       MIN(source_file) AS source_file
				FROM nodes
				WHERE source_file IS NOT NULL AND source_file != ''
				GROUP BY parent_id
			)
			SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') AS source_id,
			       n.id AS node_id,
			       0  AS start_byte,
			       0  AS end_byte,
			       0  AS start_row,
			       0  AS start_col,
			       0  AS metric
			FROM nodes n
			LEFT JOIN child_source cs ON cs.node_id = n.id
			-- Drop orphan nodes (no resolvable source_file via either
			-- the dir itself or any leaf child). These are construct
			-- dirs that survived the engine's processNode but whose
			-- file-children loop produced nothing — e.g. JS function
			-- declarations matched against an FCA-inferred Go schema
			-- where the {{.scope}} render path didn't yield writable
			-- content. They have no source data and can't be navigated
			-- to, so flagging them is pure noise.
			WHERE COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') != ''
			  AND n.id IN (SELECT DISTINCT node_id FROM v_defs)
			  AND n.id NOT IN (SELECT node_id FROM alive)
			  AND n.id NOT IN (SELECT node_id FROM skipped)
			  -- Skip non-callable categories. node_refs is populated
			  -- by call-extraction (call_expression / selector field,
			  -- plus a handful of function-value patterns for Go) —
			  -- types/constants/variables/imports are referenced by
			  -- identifier in non-call contexts and never make it
			  -- into the table, so flagging them is pure noise.
			  AND n.id NOT LIKE '%%/imports/%%'
			  AND n.id NOT LIKE 'imports/%%'
			  AND n.id NOT LIKE '%%/types/%%'
			  AND n.id NOT LIKE 'types/%%'
			  AND n.id NOT LIKE '%%/constants/%%'
			  AND n.id NOT LIKE 'constants/%%'
			  AND n.id NOT LIKE '%%/variables/%%'
			  AND n.id NOT LIKE 'variables/%%'
			  -- Generated code (capnp / protobuf / *_generated.go /
			  -- *.gen.go) intentionally exports wide APIs that the
			  -- consumer picks from — most exported methods aren't
			  -- internally called. Flagging them buries real findings
			  -- under noise (256/305 of mache's pre-filter findings
			  -- came from a single capnp.go file). Skip-list by file
			  -- suffix on the resolved source_id.
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.capnp.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.pb.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%_generated.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.gen.go'
			%s
			ORDER BY source_id, n.id
		`,
	},
	{
		ID:          "cyclomatic_complexity",
		Languages:   []string{"go"},
		Description: "Per-function cyclomatic complexity, computed as the count of control-flow AST nodes (if/for/case/select-case) inside each function or method body. Findings are sorted descending by metric — agents typically only care about the top N, so pair with `min_metric` to set a cutoff (e.g. 10 for 'noteworthy', 20 for 'review now'). Rule scopes via fn.source_id when source_id is provided.",
		Requires:    []string{"_ast"},
		ScopeColumn: "fn.source_id",
		Query: `
			SELECT fn.source_id,
			       fn.node_id,
			       fn.start_byte,
			       fn.end_byte,
			       fn.start_row,
			       fn.start_col,
			       COUNT(branch.node_id) AS metric
			FROM _ast fn
			LEFT JOIN _ast branch
			  ON branch.source_id = fn.source_id
			  AND branch.node_id LIKE fn.node_id || '/%%'
			  AND branch.node_kind IN ('if_statement','for_statement','case_clause','expression_case','type_case','communication_case','default_case')
			WHERE fn.node_kind IN ('function_declaration','method_declaration')
			%s
			GROUP BY fn.source_id, fn.node_id, fn.start_byte, fn.end_byte, fn.start_row, fn.start_col
			ORDER BY metric DESC, fn.source_id, fn.start_byte
		`,
	},
	{
		ID:          "long_function",
		Languages:   []string{"go"},
		Description: "Functions and methods sorted descending by body line count (end_row - start_row). Default threshold is 81 lines — matches the historical strict `> 80` SQL floor exactly, so agents calling without min_metric see the same long-function set as before. Pass min_metric to adjust: 200 for 'definitely review now', 120 for 'noteworthy', 0 to see everything sorted by length. Pair with cyclomatic_complexity for a sister metric on the same nodes.",
		Requires:    []string{"_ast"},
		ScopeColumn: "fn.source_id",
		// 81 not 80: the old SQL filter was strict `> 80`, so 80-line
		// functions were excluded. min_metric is `>=`, so mapping
		// `> 80` → `>= 81` preserves the boundary exactly. Tests that
		// assert "only the 100-line one fires when 80-line is at the
		// boundary" stay green.
		DefaultMinMetric: 81,
		Query: `
			SELECT fn.source_id,
			       fn.node_id,
			       fn.start_byte,
			       fn.end_byte,
			       fn.start_row,
			       fn.start_col,
			       (fn.end_row - fn.start_row) AS metric
			FROM _ast fn
			WHERE fn.node_kind IN ('function_declaration','method_declaration')
			%s
			ORDER BY metric DESC, fn.source_id, fn.start_byte
		`,
	},
	{
		ID:          "untested_function",
		Languages:   []string{"go"},
		Description: "Exported Go standalone functions (only constructs under a 'functions/' category) with no test coverage. Static proxy: a function is considered covered if EITHER a same-named 'Test<Foo>' counterpart exists in node_defs OR the function is referenced from inside any 'Test*'/'Benchmark*'/'Example*' construct's source (i.e. a test calls it). Constructors named 'NewBar' are also satisfied by any 'TestBar*' prefix match — Go convention puts constructor coverage under TestBar_<MethodName>, not TestNewBar. False positives expected for tests that exercise functions only via reflection or shell-out. Methods, types, constants, variables, and imports are skipped: Go test names use Test<Func> not Test<Receiver>.<Method>. Excludes Test*/Benchmark*/Example* tokens themselves, plus main/init. Generated code (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) is skipped. source_id falls back to a child's source_file when the construct dir doesn't carry one.",
		Requires:    []string{"node_defs", "nodes"},
		ScopeColumn: "COALESCE(NULLIF(n.source_file, ''), cs.source_file, '')",
		Query: `
			WITH child_source AS (
				SELECT parent_id AS node_id,
				       MIN(source_file) AS source_file
				FROM nodes
				WHERE source_file IS NOT NULL AND source_file != ''
				GROUP BY parent_id
			),
			-- Test-call coverage: a function is exercised if any
			-- ref to its token comes from a caller node whose
			-- construct dir name matches Test*/Benchmark*/Example*.
			-- Captures both schema shapes:
			--   functions/TestFoo/source            (FCA-inferred)
			--   pkg/functions/TestFoo/source        (go-schema)
			-- 'BenchmarkFoo' / 'ExampleFoo' / 'FuzzFoo' too.
			tested_via_call AS (
				-- Per ADR-0013 Step 4: query v_refs. The referrer column
				-- in the view is referrer_node_id (was node_id on
				-- node_refs); LSP-resolved binding rows will populate it
				-- the same way mention-fidelity rows do today, so this
				-- query naturally picks up both fidelities once Step 1
				-- (ley-line-453f7e) ships.
				SELECT DISTINCT token
				FROM v_refs
				WHERE referrer_node_id LIKE '%%/Test%%/source'
				   OR referrer_node_id LIKE 'Test%%/source'
				   OR referrer_node_id LIKE '%%/Benchmark%%/source'
				   OR referrer_node_id LIKE 'Benchmark%%/source'
				   OR referrer_node_id LIKE '%%/Example%%/source'
				   OR referrer_node_id LIKE 'Example%%/source'
				   OR referrer_node_id LIKE '%%/Fuzz%%/source'
				   OR referrer_node_id LIKE 'Fuzz%%/source'
			)
			SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') AS source_id,
			       d.node_id,
			       0 AS start_byte,
			       0 AS end_byte,
			       0 AS start_row,
			       0 AS start_col,
			       0 AS metric
			FROM v_defs d
			JOIN nodes n ON n.id = d.node_id
			LEFT JOIN child_source cs ON cs.node_id = n.id
			-- Accept either an exact 'Test<Foo>' counterpart (the
			-- canonical Go test name for func Foo) OR — when the
			-- function name starts with 'New' — any TestBar* prefix
			-- match for the constructor's bare type name. NewMemoryStore
			-- is overwhelmingly tested via TestMemoryStore_TracksMtimes
			-- and friends, never via TestNewMemoryStore.
			LEFT JOIN v_defs t
			  ON t.token = 'Test' || d.token
			  OR (
			    substr(d.token, 1, 3) = 'New'
			    AND length(d.token) > 3
			    AND t.token LIKE 'Test' || substr(d.token, 4) || '%%'
			  )
			-- Static-call test coverage. Token referenced from inside
			-- any Test*/Benchmark*/Example*/Fuzz* construct's source.
			LEFT JOIN tested_via_call tc ON tc.token = d.token
			WHERE substr(d.token, 1, 1) GLOB '[A-Z]'
			  AND d.token NOT LIKE 'Test%%'
			  AND d.token NOT LIKE 'Benchmark%%'
			  AND d.token NOT LIKE 'Example%%'
			  AND d.token NOT LIKE 'Fuzz%%'
			  -- Register* functions are by-convention init-time
			  -- registration handlers (RegisterRefQuery,
			  -- RegisterAddressRefQuery, etc.). They run via
			  -- init() in every consumer of the package, so they
			  -- ARE exercised in every test by virtue of importing
			  -- the package — but the static call-graph view ties
			  -- their callers to functions/init/source, which the
			  -- tested_via_call CTE doesn't recognize as a test
			  -- caller. Excluding the prefix avoids the FP.
			  AND d.token NOT LIKE 'Register%%'
			  AND d.token NOT IN ('main','init','String','Error')
			  AND t.token IS NULL
			  AND tc.token IS NULL
			  -- Restrict to constructs in a 'functions/' category dir.
			  -- 'functions/Foo' (auto-inferred flat shape) and
			  -- 'pkg/functions/Foo' (explicit go-schema package shape)
			  -- both match. Skips methods/, types/, constants/,
			  -- variables/, imports/ — those don't follow the
			  -- TestFoo naming convention.
			  AND (d.node_id LIKE 'functions/%%' OR d.node_id LIKE '%%/functions/%%')
			  -- Generated code (capnp / protobuf / *_generated.go /
			  -- *.gen.go) isn't expected to have a TestFoo counterpart
			  -- in the consumer's tests — skip it.
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.capnp.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.pb.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%_generated.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.gen.go'
			%s
			ORDER BY source_id, d.token
		`,
	},
	{
		ID:          "duplicate_definitions",
		Description: "Symbols defined under more than one node_id in node_defs — same token, multiple definition sites. Restricted to free functions (`functions/`); methods/ defs are excluded structurally because the engine registers a bare-leaf alias ('Method' from 'Receiver.Method') for the dead_code skip list, which causes every interface method (ReadContent, GetNode, ListChildren, ...) implemented by N types to collide as a 'duplicate' even though the implementations are conceptually distinct. The skip list further excludes interface contracts by name (String/Error/Read/Write/...) for the rare case where free functions match those names. Metric is the duplicate count, sorted descending. Excludes 'imports/' (refs to external packages, not defs), non-callable categories types/, constants/, variables/ — short names like 'content', 'src', 'name' duplicate across functions and aren't meaningful 'duplicate definitions' — and generated code (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) where wide method sets are produced by the generator and aren't 'duplication' in the architectural sense. source_id falls back to a child's source_file when the construct dir doesn't carry one. Cross-language since node_defs is populated by every leyline parser.",
		Requires:    []string{"node_defs", "nodes"},
		ScopeColumn: "COALESCE(NULLIF(n.source_file, ''), cs.source_file, '')",
		Query: `
			WITH child_source AS (
				SELECT parent_id AS node_id,
				       MIN(source_file) AS source_file
				FROM nodes
				WHERE source_file IS NOT NULL AND source_file != ''
				GROUP BY parent_id
			)
			SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') AS source_id,
			       d.node_id,
			       0 AS start_byte,
			       0 AS end_byte,
			       0 AS start_row,
			       0 AS start_col,
			       c.copies AS metric
			FROM (
				SELECT token, COUNT(*) AS copies
				FROM v_defs
				-- Skip-list match against the unqualified leaf, so
				-- both bare tokens ('init') and qualified shapes
				-- ('cmd.init', 'lang.init') get skipped uniformly.
				-- substr(token, instr(token, '.') + 1) strips the
				-- 'pkg.' prefix; instr returns 0 when there's no
				-- dot, so substr(token, 1) returns the full token.
				-- Skip standard-library and common-external-interface
				-- contracts. Duplicates of these aren't 'duplication'
				-- in the architectural sense — they're each type's
				-- implementation of a shared protocol. Mirror the
				-- dead_code skip list (PRs #234, #239) so both rules
				-- treat the same names consistently.
				WHERE substr(token, instr(token, '.') + 1) NOT IN (
					'main','init',
					'String','Error','Format','Scan','GoString',
					'Read','Write','Close','Open','Seek','ReadAt','WriteAt','ReadFrom','WriteTo',
					'Len','Less','Swap',
					'MarshalJSON','UnmarshalJSON','MarshalText','UnmarshalText','MarshalBinary','UnmarshalBinary',
					'Marshal','Unmarshal','Reset','Clone','Copy','Equal','Hash','Validate',
					-- vtab.Module (modernc.org/sqlite/vtab)
					'BestIndex','Column','Connect','Create','Destroy','Disconnect',
					'Eof','Filter','Rowid','Next',
					-- billy.Filesystem (go-git)
					'Chroot','Readlink','Sys','TempFile','MkdirAll','Symlink',
					'Lstat','Stat','ReadDir','Capabilities','Root',
					-- net/http
					'ServeHTTP'
				)
				-- Exclude imports/ (refs TO external packages, not
				-- defs) and non-callable categories (short names
				-- like 'content', 'src', 'name' duplicate across
				-- functions and aren't meaningful 'duplicates').
				AND node_id NOT LIKE '%%/imports/%%'
				AND node_id NOT LIKE 'imports/%%'
				AND node_id NOT LIKE '%%/types/%%'
				AND node_id NOT LIKE 'types/%%'
				AND node_id NOT LIKE '%%/constants/%%'
				AND node_id NOT LIKE 'constants/%%'
				AND node_id NOT LIKE '%%/variables/%%'
				AND node_id NOT LIKE 'variables/%%'
				-- Exclude method defs entirely. The Go schema's
				-- methods/ branch registers a bare-leaf alias
				-- ('Method' from 'Receiver.Method') so dead_code's
				-- skip list can match interface contracts by name —
				-- but every interface method (ReadContent, GetNode,
				-- ListChildren, ...) implemented by N types then
				-- collides on the bare token, inflating
				-- duplicate_definitions. The interface methods are
				-- conceptually distinct implementations, not
				-- duplicates. Filter them out structurally rather
				-- than enumerating an ever-growing skip list.
				AND node_id NOT LIKE '%%/methods/%%'
				AND node_id NOT LIKE 'methods/%%'
				GROUP BY token
				HAVING copies > 1
			) c
			JOIN v_defs d ON d.token = c.token
			JOIN nodes n ON n.id = d.node_id
			LEFT JOIN child_source cs ON cs.node_id = n.id
			-- Apply the same filters to the join so partial matches
			-- (where one repo's imports overlap with a real def in
			-- another repo) don't leak through.
			WHERE d.node_id NOT LIKE '%%/imports/%%'
			  AND d.node_id NOT LIKE 'imports/%%'
			  AND d.node_id NOT LIKE '%%/types/%%'
			  AND d.node_id NOT LIKE 'types/%%'
			  AND d.node_id NOT LIKE '%%/constants/%%'
			  AND d.node_id NOT LIKE 'constants/%%'
			  AND d.node_id NOT LIKE '%%/variables/%%'
			  AND d.node_id NOT LIKE 'variables/%%'
			  AND d.node_id NOT LIKE '%%/methods/%%'
			  AND d.node_id NOT LIKE 'methods/%%'
			  -- Generated code (capnp / protobuf / *_generated.go /
			  -- *.gen.go) intentionally produces wide method sets
			  -- (DecodeFromPtr, EncodeAsPtr, IsValid, Message, Segment,
			  -- ToPtr) on every generated type — those aren't
			  -- "duplicate definitions" in the architectural sense.
			  -- Match by resolved source file (falling back to a
			  -- child's source_file when the construct dir has none).
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.capnp.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.pb.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%_generated.go'
			  AND COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') NOT LIKE '%%.gen.go'
			%s
			ORDER BY metric DESC, source_id, d.node_id
		`,
	},
	{
		ID:          "god_file",
		Description: "Source files whose distinct-definition count is at least 10 AND more than 3× the project mean — a fuzzy 'god file' detector that surfaces sprawl relative to the codebase's own distribution rather than a hard line-count cutoff. Metric is the def count, sorted descending. Cross-language since node_defs is populated by every backend. source_file is resolved via the construct dir's child leaves (the schema engine attaches Origin to source / ast.json / doc, not the wrapping dir), so this works on FCA-inferred mounts too. Generated code (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) and Go test files (`*_test.go`) are excluded — generators produce wide method sets by design and test files accumulate many TestXxx / setupXxx helpers without representing architectural sprawl. Pairs with long_file (line-count via _ast) for a 'lots of code' vs 'lots of API' split.",
		Requires:    []string{"node_defs", "nodes"},
		ScopeColumn: "pf.file",
		Query: `
			-- Resolve source_file via leaf children when the dir
			-- itself has none. The schema engine attaches Origin
			-- (and thus source_file) to leaf rendered files (source,
			-- ast.json, doc) but not to the wrapping construct dir.
			-- Without this fallback per_file groups by '' (empty)
			-- and produces zero rows on the FCA-inferred path.
			WITH child_source AS (
				SELECT parent_id AS node_id,
				       MIN(source_file) AS source_file
				FROM nodes
				WHERE source_file IS NOT NULL AND source_file != ''
				GROUP BY parent_id
			),
			per_file AS (
				SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file) AS file,
				       COUNT(DISTINCT d.token) AS n
				FROM v_defs d
				JOIN nodes n ON n.id = d.node_id
				LEFT JOIN child_source cs ON cs.node_id = n.id
				WHERE COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') != ''
				GROUP BY COALESCE(NULLIF(n.source_file, ''), cs.source_file)
			),
			proj AS (
				SELECT AVG(n) AS mu FROM per_file
			)
			SELECT pf.file AS source_id,
			       '' AS node_id,
			       0 AS start_byte,
			       0 AS end_byte,
			       0 AS start_row,
			       0 AS start_col,
			       pf.n AS metric
			FROM per_file pf
			CROSS JOIN proj p
			WHERE pf.n >= 10
			  AND CAST(pf.n AS REAL) > 3.0 * p.mu
			  -- Skip generated code: capnp / protobuf / *_generated /
			  -- *.gen produce wide method sets by design, not sprawl.
			  AND pf.file NOT LIKE '%%.capnp.go'
			  AND pf.file NOT LIKE '%%.pb.go'
			  AND pf.file NOT LIKE '%%_generated.go'
			  AND pf.file NOT LIKE '%%.gen.go'
			  -- Skip Go test files. *_test.go accumulates many
			  -- TestXxx / setupXxx helpers without representing
			  -- architectural sprawl — every test func counts as a
			  -- distinct def. The test-prefix skip in dead_code /
			  -- untested_function is per-construct; god_file is
			  -- per-file, so we filter at the file extension level.
			  AND pf.file NOT LIKE '%%_test.go'
			%s
			ORDER BY metric DESC, source_id
		`,
	},
	{
		ID:          "fan_out_skew",
		Description: "Constructs whose distinct callee QUALIFIER count via v_refs is at least 5 AND more than 3× the project mean — god-functions / orchestrators that touch too many distinct receiver types or packages. Metric is COUNT(DISTINCT COALESCE(NULLIF(qualifier, ''), token)) per referrer, sorted descending. Qualifier comes from LLO's BindingRecord (T8.7) — the LHS of the selector_expression at the ref site, so projection functions calling N methods on one receiver score 1 (one qualifier) instead of N (N tokens). Pre-T8.7 records and tree-sitter mention rows have empty qualifier; the COALESCE falls back to token, preserving the pre-T8.7 metric on legacy .dbs. Language-agnostic. Skip-listed: testing-framework prefixes Test*/Benchmark*/Example*/Fuzz* (per-construct) and Go test files *_test.go (per-file). Generated code (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) is excluded — generated dispatchers naturally have wide fan-out. The previous projection-function naming-pattern exemption (mache-6c0d07's predecessor #355) is now structurally subsumed by the qualifier metric and was retired. The 3× threshold and 5-call floor are heuristics; adjust by editing the rule body.",
		Requires:    []string{"node_refs", "nodes"},
		ScopeColumn: "COALESCE(n.source_file, '')",
		Query: `
			WITH fanout AS (
				-- Exclude the engine's file-level sentinel caller_ids
				-- (prefix '_file_level:'). Those are synthetic node_ids
				-- the engine uses to mark file-level fn-value refs
				-- (mache-02r9) as alive without polluting per-construct
				-- callee counts. Counting them here would attribute the
				-- file-level refs to a 'caller' that isn't a real
				-- construct, inflating fan_out_skew with virtual rows.
				--
				-- Metric: COUNT DISTINCT qualifier per referrer.
				-- Qualifier is the LHS of the selector_expression at
				-- the ref site (per LLO BindingRecord.qualifier, T8.7).
				-- Counting distinct qualifiers — receiver/package
				-- origins — distinguishes orchestration (calls across
				-- many packages) from projection (N method calls on
				-- one receiver, N distinct tokens but ONE qualifier).
				--
				-- Pre-T8.7 records and tree-sitter mention rows have
				-- empty qualifier; the COALESCE falls back to token
				-- so the metric degrades gracefully to the pre-T8.7
				-- behavior on those records (no regression for legacy
				-- .dbs without the qualifier field).
				SELECT referrer_node_id AS caller_id,
				       COUNT(DISTINCT COALESCE(NULLIF(qualifier, ''), token)) AS n
				FROM v_refs
				WHERE referrer_node_id NOT LIKE '_file_level:%%'
				GROUP BY referrer_node_id
			),
			proj AS (
				SELECT AVG(n) AS mu FROM fanout
			)
			SELECT COALESCE(n.source_file, '') AS source_id,
			       f.caller_id AS node_id,
			       0 AS start_byte,
			       0 AS end_byte,
			       0 AS start_row,
			       0 AS start_col,
			       f.n AS metric
			FROM fanout f
			JOIN nodes n ON n.id = f.caller_id
			-- Construct directory is the parent of the source node.
			-- Match against ctor.name so the test-prefix filter checks
			-- the construct name directly, not arbitrary substrings of
			-- the path. ctor will be NULL for top-level callers (rare);
			-- the LEFT JOIN keeps those rows so the test-prefix filter
			-- only excludes when we positively identify a test name.
			LEFT JOIN nodes ctor ON ctor.id = n.parent_id
			CROSS JOIN proj p
			WHERE f.n >= 5
			  AND CAST(f.n AS REAL) > 3.0 * p.mu
			  AND COALESCE(ctor.name, '') NOT LIKE 'Test%%'
			  AND COALESCE(ctor.name, '') NOT LIKE 'Benchmark%%'
			  AND COALESCE(ctor.name, '') NOT LIKE 'Example%%'
			  AND COALESCE(ctor.name, '') NOT LIKE 'Fuzz%%'
			  -- Generated dispatchers (capnp / protobuf / *_generated /
			  -- *.gen) have intentionally wide fan-out; flagging them
			  -- buries real findings under generated-code noise.
			  AND COALESCE(n.source_file, '') NOT LIKE '%%.capnp.go'
			  AND COALESCE(n.source_file, '') NOT LIKE '%%.pb.go'
			  AND COALESCE(n.source_file, '') NOT LIKE '%%_generated.go'
			  AND COALESCE(n.source_file, '') NOT LIKE '%%.gen.go'
			  -- Go test files: test helpers (RunSuite, runParity,
			  -- setup, realWriteBack) legitimately call many things
			  -- to build fixtures. The Test*/Benchmark*/Example*/Fuzz*
			  -- per-construct filter above doesn't catch helpers
			  -- with non-test names, so add a per-file filter to
			  -- match god_file.
			  AND COALESCE(n.source_file, '') NOT LIKE '%%_test.go'
			%s
			ORDER BY metric DESC, source_id, node_id
		`,
	},
	{
		ID:          "sleep_in_test",
		Languages:   []string{"go"},
		Description: "Calls to time.Sleep / time.After / time.NewTimer / time.NewTicker / time.AfterFunc from inside Go test files (`*_test.go`). Sleep-based synchronization is a flakiness anti-pattern: tests that wait wall-clock time instead of polling an observable condition (channel close, monitor event, exit-code check) drift between machines and CI environments. Prefer `testing.T.Eventually`, `Monitor`-style notifications, or `until <cond>; do sleep 0.5; done` polling loops. Cross-backend rule (uses node_refs + nodes, no _ast required). Static heuristic — captures the bare token after call-extraction so a non-time.Sleep method on an unrelated type (e.g. a custom Sleep() on a mock) is a false positive; documented divergence rather than over-engineering. Benchmark files are still flagged — sleep-based timing measurement in benchmarks is wrong for the same flakiness reason; if you need it explicitly, exclude the file via the rule's allowlist (TBD, tracked under mache-6682ec's smell-rule-categories sub-bead).",
		Requires:    []string{"node_refs", "nodes"},
		ScopeColumn: "COALESCE(n.source_file, '')",
		Query: `
			SELECT COALESCE(n.source_file, n.id) AS source_id,
			       n.id AS node_id,
			       0 AS start_byte, 0 AS end_byte,
			       0 AS start_row, 0 AS start_col,
			       0 AS metric
			FROM node_refs r
			JOIN nodes n ON n.id = r.node_id
			WHERE r.token IN ('Sleep', 'After', 'NewTimer', 'NewTicker', 'AfterFunc')
			  AND COALESCE(n.source_file, '') LIKE '%%_test.go'
			%s
			ORDER BY source_id, node_id
		`,
	},
	{
		ID:          "long_file",
		Description: "Source files sorted descending by total line count (end_row of the source_file root AST node). Default threshold is 1501 lines — matches the historical strict `> 1500` SQL floor exactly, so agents calling without min_metric see the same long-file set as before. Pass min_metric to adjust: 3000 for 'definitely review now', 800 for 'noteworthy', 0 to see everything sorted by length. Cross-language since the rule joins by node_kind = 'source_file' which most tree-sitter grammars use as the root kind.",
		Requires:    []string{"_ast"},
		ScopeColumn: "src.source_id",
		// 1501 not 1500: old SQL `> 1500` excluded files at exactly
		// 1500 lines; min_metric is `>=`, so 1501 preserves the
		// boundary. Same pattern as long_function (#302).
		DefaultMinMetric: 1501,
		Query: `
			SELECT src.source_id,
			       src.node_id,
			       src.start_byte,
			       src.end_byte,
			       src.start_row,
			       src.start_col,
			       src.end_row AS metric
			FROM _ast src
			WHERE src.node_kind = 'source_file'
			%s
			ORDER BY metric DESC, src.source_id
		`,
	},
}

func init() {
	// Append external rules from $MACHE_SMELL_RULES_DIR so they
	// share the registry with built-ins. Delegates to
	// appendExternalRulesFromEnv so the wiring is testable —
	// init() itself can't be re-run with different env values
	// across tests, but the helper takes the value as a param.
	// Errors are already logged inside the helper; init() just
	// drops the return value.
	_, _ = appendExternalRulesFromEnv(os.Getenv(SmellRulesEnvVar))
}

// appendExternalRulesFromEnv loads external rules from the given
// directory path (typically `$MACHE_SMELL_RULES_DIR`) and appends
// them to smellRegistry. A load error logs and skips the
// extension entirely — mache stays up so a typo in an external
// rule can't take down the server. Tests call
// LoadExternalSmellRules directly to assert errors loudly; this
// helper exercises the full append + log path.
//
// Returns (added, err): the count of appended rules and any
// load error. err is nil-on-empty-dir (no env, no error). Tests
// inspect the return; init() ignores it (logs at the source).
func appendExternalRulesFromEnv(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	rules, err := LoadExternalSmellRules(dir)
	if err != nil {
		log.Printf("smell rules: skipping external rules in %s: %v", dir, err)
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}
	smellRegistry = append(smellRegistry, rules...)
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	log.Printf("smell rules: loaded %d external rule(s) from %s: %s",
		len(rules), dir, strings.Join(ids, ", "))
	return len(rules), nil
}

// smellFinding is one row of a smell scan. Byte ranges and (1-based)
// line/column let editors jump straight to the offending span. Metric
// carries a numeric score for rules that compute one (cyclomatic
// complexity, fan-out count, line length, etc.); binary rules emit 0.
type smellFinding struct {
	RuleID    string `json:"rule_id"`
	SourceID  string `json:"source_id"`
	NodeID    string `json:"node_id,omitempty"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Line      int    `json:"line"`              // 1-based
	Column    int    `json:"column"`            // 1-based
	Metric    int64  `json:"metric,omitempty"`  // rule-specific score (0 omitted)
	Snippet   string `json:"snippet,omitempty"` // short source preview
}

// makeFindSmellsHandler returns the MCP handler. With no `rule` arg
// it lists all registered rules; with a rule it runs the scan.
func makeFindSmellsHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ruleID := strings.TrimSpace(request.GetString("rule", ""))
		sourceID := strings.TrimSpace(request.GetString("source_id", ""))
		limitFloat := request.GetFloat("limit", 200)
		limit := int(limitFloat)
		if limit <= 0 {
			limit = 200
		}

		if ruleID == "" {
			// Discovery mode — list rules.
			return mcp.NewToolResultText(jsonOrPanic(rulesListing())), nil
		}

		var rule *SmellRule
		for i := range smellRegistry {
			if smellRegistry[i].ID == ruleID {
				rule = &smellRegistry[i]
				break
			}
		}
		if rule == nil {
			return mcp.NewToolResultError(fmt.Sprintf(
				"unknown rule %q — available: %s. Call this tool with no rule for full descriptions.",
				ruleID, strings.Join(allRuleIDs(), ", "),
			)), nil
		}

		// Resolve min_metric AFTER rule lookup so we can fall back
		// to the rule's DefaultMinMetric when the request omits it.
		// An explicit `min_metric=0` from the caller still wins —
		// it disables the default and returns everything sorted by
		// metric. Only an absent key activates the default.
		minMetric := int64(0)
		if v, ok := request.GetArguments()["min_metric"]; ok {
			if f, ok := v.(float64); ok {
				minMetric = int64(f)
			}
		} else {
			minMetric = rule.DefaultMinMetric
		}

		// Need a *sql.DB to run the rule. Today we hand-shake via the
		// refsQuerier interface that other handlers already use.
		qg, ok := g.(refsQuerier)
		if !ok {
			return mcp.NewToolResultError("the active graph backend doesn't expose a SQL handle; find_smells requires a leyline-parsed .db"), nil
		}

		// Pre-flight: every rule declares the tables it reads in
		// rule.Requires. If any are missing on this backend, return a
		// friendly tool error instead of letting the SQL fail with
		// "no such table" — agents can then call the tool again with
		// a different rule, or stop.
		if missing, err := missingTables(qg, rule.Requires); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("rule %q: pre-flight table check failed: %v", ruleID, err)), nil
		} else if len(missing) > 0 {
			// If the active .db was built by `mache build`, the
			// _mache_meta marker tells us which backend produced it.
			// Splice that into the error so agents don't have to run
			// `.tables` to figure out why _ast is missing.
			backendNote := ""
			if backend := queryBuildBackend(qg); backend != "" {
				backendNote = fmt.Sprintf(" (this .db was built with backend=%q)", backend)
			}
			return mcp.NewToolResultError(fmt.Sprintf(
				"rule %q requires SQL tables [%s] which aren't present on the active backend%s. _ast / _source / _imports / _lsp* are produced by ley-line-open's `leyline parse`; node_defs / node_refs / nodes are produced by both standalone mache and LLO. See docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full table.",
				ruleID, strings.Join(missing, ", "), backendNote)), nil
		}

		findings, err := runSmellRule(qg, rule, sourceID, limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("rule %q: %v", ruleID, err)), nil
		}

		// Server-side metric threshold. Drops findings below the cutoff
		// before snippet population so we don't waste reads on rows the
		// agent will discard. min_metric=0 keeps the historical default.
		if minMetric > 0 {
			filtered := findings[:0]
			for _, f := range findings {
				if f.Metric >= minMetric {
					filtered = append(filtered, f)
				}
			}
			findings = filtered
		}

		// Snippet population is best-effort. If `_source` isn't there
		// (older .dbs) we just leave Snippet empty.
		populateSnippets(qg, findings)

		resp := struct {
			Rule     string         `json:"rule"`
			Total    int            `json:"total"`
			Findings []smellFinding `json:"findings"`
		}{
			Rule:     rule.ID,
			Total:    len(findings),
			Findings: findings,
		}
		return mcp.NewToolResultText(jsonOrPanic(resp)), nil
	}
}

// allRuleIDs returns the registered rule IDs in alphabetical order.
// Used by the unknown-rule error message so agents/users see what
// they could have typed without having to call the registry first.
func allRuleIDs() []string {
	ids := make([]string, 0, len(smellRegistry))
	for _, r := range smellRegistry {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// rulesListing produces the JSON returned when find_smells is called
// without a rule. Stable order so agents can cache the listing.
func rulesListing() any {
	type ruleSummary struct {
		ID               string   `json:"id"`
		Languages        []string `json:"languages,omitempty"`
		Description      string   `json:"description"`
		Requires         []string `json:"requires,omitempty"`
		DefaultMinMetric int64    `json:"default_min_metric,omitempty"`
	}
	out := make([]ruleSummary, 0, len(smellRegistry))
	for _, r := range smellRegistry {
		out = append(out, ruleSummary{
			ID:               r.ID,
			Languages:        r.Languages,
			Description:      r.Description,
			Requires:         r.Requires,
			DefaultMinMetric: r.DefaultMinMetric,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return struct {
		Help  string        `json:"help"`
		Rules []ruleSummary `json:"rules"`
	}{
		Help:  "find_smells runs structural pattern queries against the _ast / nodes / node_defs / node_refs tables. Pass `rule` to scan; omit it (this response) to list available rules. Each rule entry includes a `requires` list of SQL tables it reads — agents can use it to skip rules whose tables aren't present on the active backend (e.g. _ast is only populated by ley-line-open's leyline parse). Optional `source_id` filters to one parsed file; `limit` caps results (default 200); `min_metric` drops findings whose metric column is below the threshold. Rules that surface a `default_min_metric` apply that as the threshold when the caller omits `min_metric`; an explicit `min_metric=0` overrides the default and returns everything sorted by metric.",
		Rules: out,
	}
}

// queryBuildBackend returns the value of `_mache_meta.backend` on the
// active backend, or "" if the table isn't present, the row is missing,
// or any error occurs. `mache build` stamps this marker for every .db
// it produces (see cmd/build_meta.go); older or third-party-produced
// .dbs return "" silently so the caller can fall back to a generic
// message. Best-effort by design — never returns an error.
func queryBuildBackend(qg refsQuerier) string {
	rows, err := qg.QueryRefs(`SELECT value FROM _mache_meta WHERE key = 'backend'`)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return ""
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		return ""
	}
	return v
}

// missingTables returns the subset of `required` that's not in
// sqlite_master on the active backend. Returns nil if everything is
// present (or `required` is empty). The query uses placeholders for
// the IN list so it works with arbitrary SQLite drivers.
func missingTables(qg refsQuerier, required []string) ([]string, error) {
	if len(required) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(required))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(required))
	for i, t := range required {
		args[i] = t
	}
	rows, err := qg.QueryRefs(
		"SELECT name FROM sqlite_master WHERE type='table' AND name IN ("+placeholders+")",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(required))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, t := range required {
		if _, ok := present[t]; !ok {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

// ensureCanonicalViews installs v_defs / v_refs (ADR-0013 Step 3) as
// TEMP views scoped to the active connection. Probes for ley-line-open's
// post-Step-1 _lsp_* columns (referrer_node_id, ref_token, def_token)
// and includes a UNION ALL with binding-fidelity rows when those
// columns are available; otherwise falls back to mention-only.
//
// Why TEMP views rather than persistent ones:
//
//   - SQLiteGraph opens .dbs read-only (mode=ro). Persistent DDL fails;
//     TEMP objects live in a per-connection in-memory schema and work
//     even on RO connections.
//   - The view body depends on what columns the producer wrote. With
//     TEMP views we can recompute the body per-connection rather than
//     locking in a stale shape on disk.
//   - Temp objects shadow same-named main-schema objects for the
//     current connection, so the legacy persistent mention-only views
//     installed by NewSQLiteWriter (PR #341) get overridden cleanly
//     without a migration.
//
// Idempotent — DROP VIEW IF EXISTS before each CREATE — so safe to
// call before every rule execution.
func ensureCanonicalViews(qg refsQuerier) error {
	// _lsp_defs probe still runs because v_defs's binding-fidelity
	// arm has not yet migrated to capnp — BindingRecord covers refs
	// (the link from a call site to a definition), not standalone
	// def metadata. _lsp_defs migration is tracked separately; for
	// now the def-side dual-read mirrors the pre-mache-6bd4d8 shape.
	hasLSPDefsToken, err := tableHasColumn(qg, "_lsp_defs", "def_token")
	if err != nil {
		return fmt.Errorf("probe _lsp_defs: %w", err)
	}

	defsBody := `SELECT token, node_id, 'mention' AS fidelity FROM node_defs`
	if hasLSPDefsToken {
		defsBody += `
			UNION ALL
			SELECT def_token AS token, node_id, 'binding' AS fidelity
			FROM _lsp_defs
			WHERE def_token != ''`
	}

	// v_refs columns: referrer_node_id, token, target_node_id,
	// ref_uri, ref_line, fidelity, qualifier. The qualifier column
	// is empty for mention-fidelity rows (tree-sitter call extractor
	// doesn't populate it) and for pre-T8.7 capnp records (default
	// '' per schema-evolution invariant). See mache-6c0d07 for the
	// fan_out_skew metric that consumes it.
	refsBody := `SELECT node_id AS referrer_node_id,
	       token,
	       NULL  AS target_node_id,
	       NULL  AS ref_uri,
	       NULL  AS ref_line,
	       'mention' AS fidelity,
	       ''    AS qualifier
	FROM node_refs`
	// Binding-fidelity rows come from the per-connection
	// _capnp_binding_refs TEMP table, populated from the sibling
	// .bindings.capnp event log by LoadCapnpBindings (mache-190508).
	// When LoadCapnpBindings hasn't run for this connection, the
	// table stays empty and v_refs surfaces only mention-fidelity
	// rows from node_refs — same as the pre-LSP behavior.
	//
	// The legacy _lsp_refs SQL UNION arm was retired in mache-6bd4d8
	// (T8.8 mirror): SQL columns are no longer the consumer-side
	// contract, the capnp event log is. Removing the arm structurally
	// precludes be6136-class column-name-as-protocol disagreements
	// between LLO writer and mache reader (rather than only
	// preventing them by flag). LLO continues writing _lsp_refs in
	// the transition window; mache no longer reads it.
	refsBody += `
		UNION ALL
		SELECT referrer_node_id,
		       token,
		       target_node_id,
		       ref_uri,
		       ref_line,
		       'binding' AS fidelity,
		       qualifier
		FROM _capnp_binding_refs`

	stmts := []string{
		"DROP VIEW IF EXISTS temp.v_defs",
		"DROP VIEW IF EXISTS temp.v_refs",
		"DROP TABLE IF EXISTS temp._capnp_binding_refs",
		// qualifier column added in mache-6c0d07 (T8.7 mirror).
		// Empty string when LLO didn't see a selector_expression
		// upstream of the ref site, OR when the record came from a
		// pre-T8.7 .bindings.capnp log. fan_out_skew uses
		// COALESCE(NULLIF(qualifier, ''), token) so both shapes
		// degrade gracefully.
		`CREATE TEMP TABLE _capnp_binding_refs (
			referrer_node_id TEXT NOT NULL,
			token TEXT NOT NULL,
			target_node_id TEXT,
			ref_uri TEXT,
			ref_line INTEGER,
			qualifier TEXT NOT NULL DEFAULT ''
		)`,
		"CREATE TEMP VIEW v_defs AS " + defsBody,
		"CREATE TEMP VIEW v_refs AS " + refsBody,
	}
	for _, s := range stmts {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure canonical views: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// LoadCapnpBindings populates the per-connection _capnp_binding_refs
// TEMP table from the sibling .bindings.capnp event log of dbPath.
// Must be called AFTER ensureCanonicalViews on the same connection
// (the TEMP table is created there).
//
// No-op when the sibling log is missing (returns nil) — the canonical
// view's UNION arm just stays empty. Returns an error when the log
// exists but is corrupt.
//
// Producers that write to BOTH _lsp_refs AND .bindings.capnp will
// produce duplicate-shaped rows in v_refs (one per producer); set-
// membership consumers (alive-check in dead_code) deduplicate
// naturally, so this isn't a correctness issue. Once the capnp event
// log is the canonical producer (post-T8.5), the _lsp_refs UNION arm
// in ensureCanonicalViews can be removed.
func LoadCapnpBindings(qg refsQuerier, dbPath string) error {
	if dbPath == "" {
		return nil
	}
	logPath := lsp.SiblingBindingLogPath(dbPath)
	records, err := lsp.ReadBindingLog(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load capnp bindings: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	// Single-row INSERTs are ~30K syscalls on a typical mache-scale
	// log. Acceptable for the first pass; if it shows up in profiles
	// switch to a multi-row INSERT or a prepared-stmt loop.
	for _, r := range records {
		stmt := `INSERT INTO _capnp_binding_refs
			(referrer_node_id, token, target_node_id, ref_uri, ref_line, qualifier)
			VALUES (?, ?, ?, ?, ?, ?)`
		rows, err := qg.QueryRefs(stmt, r.ConstructNodeID, r.RefToken,
			r.TargetNodeID, r.RefURI, int64(r.RefRange.StartLine), r.Qualifier)
		if err != nil {
			return fmt.Errorf("insert capnp binding: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// tableHasColumn returns true iff the given table exists AND contains
// a column of the given name. Implemented via PRAGMA table_info, which
// returns zero rows for missing tables (rather than erroring), so this
// helper collapses "table missing" and "column missing" into false.
//
// Used by ensureCanonicalViews to decide whether to add the binding-
// fidelity UNION ALL clause to the v_defs / v_refs body. A pre-Step-1
// _lsp_refs table (without referrer_node_id / ref_token) reads as
// "no binding-fidelity rows available" and the views fall back to
// mention-only — same shape as today.
func tableHasColumn(qg refsQuerier, table, col string) (bool, error) {
	// Table name is interpolated directly; PRAGMA table_info doesn't
	// accept positional parameters. table comes from a hardcoded
	// constant in this file (not user input), so injection risk is
	// nil — but assert anyway via a defensive check.
	if !isSimpleIdent(table) {
		return false, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := qg.QueryRefs(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		// SQLite returns rows (possibly zero) for valid PRAGMA calls
		// whether or not the table exists; an error here is genuine.
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// isSimpleIdent guards against SQL injection in PRAGMA table_info
// where the table name can't be parameterized. Allows ASCII letters,
// digits, and underscores — the shape of every table mache writes.
func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// dbPathProvider is the opt-in interface a refsQuerier implements
// when it knows its backing .db file path. The path is only used to
// locate the sibling .bindings.capnp event log for capnp-readthrough
// (mache-190508 step 3); queriers that don't know the path (in-memory
// test fixtures, the in-process arena) skip the capnp source
// silently. The mention + legacy SQL binding paths still apply.
type dbPathProvider interface {
	DBPath() string
}

// runSmellRule executes the rule's SQL, optionally scoped to one source.
func runSmellRule(qg refsQuerier, rule *SmellRule, sourceID string, limit int) ([]smellFinding, error) {
	if err := ensureCanonicalViews(qg); err != nil {
		return nil, err
	}
	// Capnp readthrough (mache-190508 step 3): if this querier knows
	// its dbPath AND a sibling .bindings.capnp event log exists, load
	// its records into the per-connection _capnp_binding_refs TEMP
	// table. v_refs is already configured to UNION over that table
	// (in ensureCanonicalViews); empty table → no extra rows.
	if dp, ok := qg.(dbPathProvider); ok {
		if err := LoadCapnpBindings(qg, dp.DBPath()); err != nil {
			return nil, err
		}
	}

	scope := ""
	args := []any{}
	if sourceID != "" && rule.ScopeColumn != "" {
		scope = "AND " + rule.ScopeColumn + " = ?"
		args = append(args, sourceID)
	}
	query := fmt.Sprintf(rule.Query, scope) + fmt.Sprintf(" LIMIT %d", limit)

	rows, err := qg.QueryRefs(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []smellFinding
	for rows.Next() {
		var (
			src       string
			nodeID    string
			startByte int
			endByte   int
			startRow  int
			startCol  int
			metric    int64
		)
		if err := rows.Scan(&src, &nodeID, &startByte, &endByte, &startRow, &startCol, &metric); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, smellFinding{
			RuleID:    rule.ID,
			SourceID:  src,
			NodeID:    nodeID,
			StartByte: startByte,
			EndByte:   endByte,
			Line:      startRow + 1, // tree-sitter is 0-indexed; agents expect 1-based
			Column:    startCol + 1,
			Metric:    metric,
		})
	}
	return out, rows.Err()
}

// populateSnippets fills the Snippet field on each finding. We batch by
// source_id so a file's _source bytes are read at most once.
func populateSnippets(qg refsQuerier, findings []smellFinding) {
	if len(findings) == 0 {
		return
	}
	sources := make(map[string][]byte)
	for i := range findings {
		src, hit := sources[findings[i].SourceID]
		if !hit {
			rows, err := qg.QueryRefs(
				"SELECT content FROM _source WHERE id = ?", findings[i].SourceID,
			)
			if err != nil {
				sources[findings[i].SourceID] = nil
				continue
			}
			if rows.Next() {
				var content []byte
				if scanErr := rows.Scan(&content); scanErr == nil {
					src = content
				}
			}
			_ = rows.Close()
			sources[findings[i].SourceID] = src
		}
		if src == nil {
			continue
		}
		const padding = 30
		s := max(findings[i].StartByte-padding, 0)
		e := min(findings[i].EndByte+padding, len(src))
		if s < e {
			snippet := string(src[s:e])
			snippet = strings.ReplaceAll(snippet, "\n", " ")
			findings[i].Snippet = strings.TrimSpace(snippet)
		}
	}
}

func jsonOrPanic(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": "marshal: %v"}`, err)
	}
	return string(b)
}
