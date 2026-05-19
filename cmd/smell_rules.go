package cmd

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
	// Severity controls whether a finding from this rule is fatal to
	// the gate. Three tiers per ESLint precedent (off/warn/error) —
	// see ADR-0018 and bead mache-ec1a06 for the prior-art rationale.
	//
	//   - "off"   — rule defined but never produces findings; useful
	//     for shipping a rule in a pack that's disabled for now
	//   - "warn"  — emits findings, exit 0 (observability default)
	//   - "error" — emits findings, exit 1 (rule author asserts this
	//     drift must not ship)
	//
	// Default ("") is treated as "warn" — preserves the existing
	// observability contract for all rules that haven't opted in.
	// Gate decision is made at CLI invocation via --fail-on (pylint
	// precedent): rule says what's true, gate decides whether truth
	// is fatal.
	Severity Severity
	// Tags is a free-form classification used for CLI selection via
	// --tags=foo,bar. Per ADR-0018 + research, NOT a closed enum:
	// stages (pre-commit, ci) emerge as CLI profiles from
	// (--tags × --fail-on) combinations, not from a fixed schema.
	// Cap at 3-5 tags per rule to avoid the clippy-group explosion;
	// expand the vocabulary deliberately, not reactively.
	Tags []string
}

// Severity is one of off / warn / error. ESLint precedent (string
// names, no numeric aliases). See mache-ec1a06 for the prior-art
// rationale across ruff/pylint/eslint/clippy/semgrep.
type Severity string

const (
	SeverityOff   Severity = "off"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Effective returns the resolved severity, treating the zero value
// ("") as warn — the default for any rule that hasn't opted in.
// Callers asking "is this rule fatal" should always go through
// Effective() rather than reading Severity directly.
func (r *SmellRule) Effective() Severity {
	switch r.Severity {
	case SeverityOff, SeverityError:
		return r.Severity
	default:
		return SeverityWarn
	}
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
		Description: "Calls to time.Sleep / time.After / time.NewTimer / time.NewTicker / time.AfterFunc from inside Go test files (`*_test.go`). Sleep-based synchronization is a flakiness anti-pattern: tests that wait wall-clock time instead of polling an observable condition (channel close, monitor event, exit-code check) drift between machines and CI environments. Prefer `testing.T.Eventually`, `Monitor`-style notifications, or `until <cond>; do sleep 0.5; done` polling loops. Cross-backend rule (uses node_refs + nodes, no _ast required). Static heuristic — captures the bare token after call-extraction so a non-time.Sleep method on an unrelated type (e.g. a custom Sleep() on a mock) is a false positive; documented divergence rather than over-engineering. Benchmark files are still flagged — sleep-based timing measurement in benchmarks is wrong for the same flakiness reason; if you need it explicitly, exclude the file via the rule's allowlist (TBD, tracked under mache-96341d).",
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
	// drift_doc_* rules — v1 placeholders per ADR-0018 (PR 3 of the
	// migration path). The rule METADATA ships now so:
	//
	//   1. `mache find_smells` (no rule) surfaces them in the listing
	//      — discovery surface is complete.
	//   2. PR 2's `--rule 'drift_doc_*'` glob has something to match —
	//      the CLI feature can be exercised end-to-end against real
	//      registry entries instead of an empty set.
	//   3. PR 4's pre-commit hook can wire the rule pack today without
	//      waiting on the actual firing logic.
	//
	// Each rule's QUERY is intentionally a no-op (WHERE 1=0). The real
	// firing logic for each has an honest dependency on additional
	// plumbing — a markdown-token preprocessor, host-side filesystem
	// validation, and a TOML config loader respectively — none of which
	// belongs in PR 3's scope. Each rule has a follow-up bead filed
	// against ADR-0018 (mache-e1b6c8) + the schema bead (mache-ec1a06).
	//
	// The placeholder Query shape — seven canonical columns wrapped in
	// WHERE 1=0 — keeps these rules wired through the existing
	// runSmellRule machinery without special-casing. They return zero
	// findings, but every other code path (listing, severity, tags,
	// requires pre-flight) treats them exactly like any other rule.
	{
		ID:        "drift_doc_dead_symbol_reference",
		Languages: []string{"markdown"},
		Description: "v1 placeholder (ADR-0018 PR 3). Spec: every backtick-fenced token `Foo` or `pkg.Bar` " +
			"in markdown should refer to a real symbol defined in node_defs; a missing definition " +
			"signals the doc has drifted from the code (renamed/removed function/type still " +
			"referenced in docs). Current implementation: no findings — the firing logic requires " +
			"a backtick-token preprocessor (either a derived _doc_tokens view populated by the " +
			"markdown ingestion path, or a SQLite regex extension) which is tracked as a separate " +
			"follow-up bead under mache-e1b6c8. The rule appears in the listing today so PR 2's " +
			"--rule 'drift_doc_*' glob has something to match and PR 4's pre-commit hook can wire " +
			"the rule pack ahead of the firing logic landing.",
		Severity:    SeverityWarn,
		Tags:        []string{"docs", "drift"},
		Requires:    []string{"nodes", "node_defs"},
		ScopeColumn: "COALESCE(md.source_file, '')",
		Query: `
			-- Placeholder: returns zero rows. Real implementation needs
			-- a derived view of backtick-fenced tokens extracted from
			-- markdown source. Follow-up bead under mache-e1b6c8.
			SELECT '' AS source_id, '' AS node_id,
			       0 AS start_byte, 0 AS end_byte,
			       0 AS start_row, 0 AS start_col,
			       0 AS metric
			WHERE 1=0
			%s
		`,
	},
	{
		ID:        "drift_doc_broken_internal_link",
		Languages: []string{"markdown"},
		Description: "v1 placeholder (ADR-0018 PR 3). Spec: every markdown internal link " +
			"`[text](relative/path)` should resolve to an existing file on disk (external URLs — " +
			"http/https/mailto — are skipped). Catches doc reorganizations that left stale links. " +
			"Current implementation: no findings — SQL cannot stat the filesystem, so the firing " +
			"logic requires a host-side post-processing step in runSmellRule that checks each " +
			"candidate link's path on disk. That's a small refactor tracked as a separate " +
			"follow-up bead under mache-e1b6c8. The rule appears in the listing today so PR 2's " +
			"--rule 'drift_doc_*' glob has something to match and PR 4's pre-commit hook can wire " +
			"the rule pack ahead of the firing logic landing.",
		Severity:    SeverityWarn,
		Tags:        []string{"docs", "drift", "links"},
		Requires:    []string{"nodes"},
		ScopeColumn: "COALESCE(md.source_file, '')",
		Query: `
			-- Placeholder: returns zero rows. Real implementation needs
			-- host-side filesystem stat for each markdown link target;
			-- pure SQL cannot do this. Follow-up bead under mache-e1b6c8.
			SELECT '' AS source_id, '' AS node_id,
			       0 AS start_byte, 0 AS end_byte,
			       0 AS start_row, 0 AS start_col,
			       0 AS metric
			WHERE 1=0
			%s
		`,
	},
	{
		ID:        "drift_doc_outdated_count",
		Languages: []string{"markdown"},
		Description: "v1 placeholder (ADR-0018 PR 3). Spec: numeric claims in markdown like " +
			"'supports 28 languages' or '17 MCP tools' should match a SQL ground-truth query the " +
			"repo author provides in .mache/drift-counts.toml (the schema is sketched in ADR-0018 " +
			"Open Question 2). Generalizes the cloister-style narrow-lint pattern already " +
			"implemented in bash as check #4 in scripts/docs-lint.sh for language-count claims. " +
			"Current implementation: no findings — the firing logic needs a TOML loader, a regex " +
			"extractor for '(\\d+) (tools|languages|rules)' patterns in markdown, and a per-claim " +
			"SQL execution loop. Those three pieces are tracked as a separate follow-up bead " +
			"under mache-e1b6c8. The rule appears in the listing today so PR 2's " +
			"--rule 'drift_doc_*' glob has something to match and PR 4's pre-commit hook can wire " +
			"the rule pack ahead of the firing logic landing.",
		Severity:    SeverityWarn,
		Tags:        []string{"docs", "drift", "counts"},
		Requires:    []string{"nodes"},
		ScopeColumn: "COALESCE(md.source_file, '')",
		Query: `
			-- Placeholder: returns zero rows. Real implementation needs
			-- a .mache/drift-counts.toml loader, a regex extractor for
			-- numeric claims in markdown, and a per-claim SQL execution
			-- loop. Follow-up bead under mache-e1b6c8.
			SELECT '' AS source_id, '' AS node_id,
			       0 AS start_byte, 0 AS end_byte,
			       0 AS start_row, 0 AS start_col,
			       0 AS metric
			WHERE 1=0
			%s
		`,
	},
}
