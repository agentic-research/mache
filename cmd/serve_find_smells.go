package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
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
			WITH alive AS (
				SELECT DISTINCT d.node_id
				FROM node_defs d
				JOIN node_refs r
				  ON r.token = d.token
				  OR (instr(d.token, '.') > 0 AND r.token = substr(d.token, instr(d.token, '.') + 1))
			),
			-- Skip-listed: any token of a construct matches a skip rule.
			-- ANY-MATCH is the right semantic — a function with both
			-- 'TestFoo' and 'pkg.TestFoo' should be skipped on the
			-- testing-framework rule even if only one row triggers.
			--
			-- Token list includes:
			--   • entry points: main, init
			--   • io.Reader/Writer/Closer: Read, Write, Close
			--   • fmt: String, Error, Format, Scan, GoString
			--   • sort.Interface: Len, Less, Swap
			--   • encoding: MarshalJSON, UnmarshalJSON
			--   • SQLite virtual-table (modernc.org/sqlite/vtab):
			--     BestIndex, Column, Connect, Create, Destroy,
			--     Disconnect, Eof, Filter, Rowid, Open, Next
			--   • billy.Filesystem (go-git): Chroot, Readlink, Sys,
			--     TempFile, MkdirAll, Symlink, Lstat, Stat, ReadDir,
			--     Capabilities, Root
			--   • net/http: ServeHTTP
			--
			-- These are interface contracts invoked by external
			-- runtimes / libraries; static call extraction can't see
			-- the dispatch site, so the methods always look dead.
			skipped AS (
				SELECT DISTINCT node_id FROM node_defs
				WHERE token IN (
					'main','init',
					'String','Error','Read','Write','Close','Len','Less','Swap',
					'MarshalJSON','UnmarshalJSON','Format','Scan','GoString',
					'BestIndex','Column','Connect','Create','Destroy','Disconnect',
					'Eof','Filter','Rowid','Open','Next',
					'Chroot','Readlink','Sys','TempFile','MkdirAll','Symlink',
					'Lstat','Stat','ReadDir','Capabilities','Root',
					'ServeHTTP'
				)
				   -- Strip any 'pkg.' qualifier when matching prefixes.
				   -- instr returns 0 if there's no dot; substr(token, 1)
				   -- is the full token, so bare and qualified shapes both
				   -- get the same leaf check.
				   OR substr(token, instr(token, '.') + 1) LIKE 'Test%%'
				   OR substr(token, instr(token, '.') + 1) LIKE 'Benchmark%%'
				   OR substr(token, instr(token, '.') + 1) LIKE 'Example%%'
				   OR substr(token, instr(token, '.') + 1) LIKE 'Fuzz%%'
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
			  AND n.id IN (SELECT DISTINCT node_id FROM node_defs)
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
		Description: "Functions and methods whose body spans more than 80 source lines (end_row - start_row). Sorted descending by line count. Threshold is hard-coded today — use cyclomatic_complexity for a sister metric on the same nodes.",
		Requires:    []string{"_ast"},
		ScopeColumn: "fn.source_id",
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
			  AND (fn.end_row - fn.start_row) > 80
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
				SELECT DISTINCT token
				FROM node_refs
				WHERE node_id LIKE '%%/Test%%/source'
				   OR node_id LIKE 'Test%%/source'
				   OR node_id LIKE '%%/Benchmark%%/source'
				   OR node_id LIKE 'Benchmark%%/source'
				   OR node_id LIKE '%%/Example%%/source'
				   OR node_id LIKE 'Example%%/source'
				   OR node_id LIKE '%%/Fuzz%%/source'
				   OR node_id LIKE 'Fuzz%%/source'
			)
			SELECT COALESCE(NULLIF(n.source_file, ''), cs.source_file, '') AS source_id,
			       d.node_id,
			       0 AS start_byte,
			       0 AS end_byte,
			       0 AS start_row,
			       0 AS start_col,
			       0 AS metric
			FROM node_defs d
			JOIN nodes n ON n.id = d.node_id
			LEFT JOIN child_source cs ON cs.node_id = n.id
			-- Accept either an exact 'Test<Foo>' counterpart (the
			-- canonical Go test name for func Foo) OR — when the
			-- function name starts with 'New' — any TestBar* prefix
			-- match for the constructor's bare type name. NewMemoryStore
			-- is overwhelmingly tested via TestMemoryStore_TracksMtimes
			-- and friends, never via TestNewMemoryStore.
			LEFT JOIN node_defs t
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
				FROM node_defs
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
			JOIN node_defs d ON d.token = c.token
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
				FROM node_defs d
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
		Description: "Constructs whose distinct callee count via node_refs is at least 5 AND more than 3× the project mean — likely god-functions / orchestrators that touch too many neighbors. Metric is the fan-out count, sorted descending. Language-agnostic (every leyline parser populates node_refs by token). Skip-listed: testing-framework prefixes Test*/Benchmark*/Example*/Fuzz* (per-construct) and Go test files *_test.go (per-file) — tests and test helpers are expected to call many things, no signal there. Generated code (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) is excluded — generated dispatchers naturally have wide fan-out. The 3× threshold and 5-call floor are heuristics; adjust by editing the rule body. Pairs with get_communities for 'this construct sprawls across community boundaries' analysis.",
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
				SELECT node_id AS caller_id, COUNT(DISTINCT token) AS n
				FROM node_refs
				WHERE node_id NOT LIKE '_file_level:%%'
				GROUP BY node_id
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
		ID:          "long_file",
		Description: "Source files exceeding 1500 lines (end_row reported on the source-file root AST node). Cross-language since the rule joins by node_kind = 'source_file' which most tree-sitter grammars use as the root kind. Threshold hard-coded today.",
		Requires:    []string{"_ast"},
		ScopeColumn: "src.source_id",
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
			  AND src.end_row > 1500
			%s
			ORDER BY metric DESC, src.source_id
		`,
	},
}

func init() {
	// Append external rules from $MACHE_SMELL_RULES_DIR so they
	// share the registry with built-ins. A parse error logs and
	// skips — mache stays up so a typo in an external rule can't
	// take down the server. Tests load rules directly via
	// LoadExternalSmellRules to assert errors loudly.
	dir := os.Getenv(SmellRulesEnvVar)
	if dir == "" {
		return
	}
	rules, err := LoadExternalSmellRules(dir)
	if err != nil {
		log.Printf("smell rules: skipping external rules in %s: %v", dir, err)
		return
	}
	if len(rules) > 0 {
		smellRegistry = append(smellRegistry, rules...)
		ids := make([]string, len(rules))
		for i, r := range rules {
			ids[i] = r.ID
		}
		log.Printf("smell rules: loaded %d external rule(s) from %s: %s",
			len(rules), dir, strings.Join(ids, ", "))
	}
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
		minMetric := int64(request.GetFloat("min_metric", 0))

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
			return mcp.NewToolResultError(fmt.Sprintf(
				"rule %q requires SQL tables [%s] which aren't present on the active backend. _ast / _source / _imports / _lsp* are produced by ley-line-open's `leyline parse`; node_defs / node_refs / nodes are produced by both standalone mache and LLO. See docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full table.",
				ruleID, strings.Join(missing, ", "))), nil
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
		ID          string   `json:"id"`
		Languages   []string `json:"languages,omitempty"`
		Description string   `json:"description"`
		Requires    []string `json:"requires,omitempty"`
	}
	out := make([]ruleSummary, 0, len(smellRegistry))
	for _, r := range smellRegistry {
		out = append(out, ruleSummary{
			ID:          r.ID,
			Languages:   r.Languages,
			Description: r.Description,
			Requires:    r.Requires,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return struct {
		Help  string        `json:"help"`
		Rules []ruleSummary `json:"rules"`
	}{
		Help:  "find_smells runs structural pattern queries against the _ast / nodes / node_defs / node_refs tables. Pass `rule` to scan; omit it (this response) to list available rules. Each rule entry includes a `requires` list of SQL tables it reads — agents can use it to skip rules whose tables aren't present on the active backend (e.g. _ast is only populated by ley-line-open's leyline parse). Optional `source_id` filters to one parsed file; `limit` caps results (default 200); `min_metric` drops findings whose metric column is below the threshold (default 0).",
		Rules: out,
	}
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

// runSmellRule executes the rule's SQL, optionally scoped to one source.
func runSmellRule(qg refsQuerier, rule *SmellRule, sourceID string, limit int) ([]smellFinding, error) {
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
