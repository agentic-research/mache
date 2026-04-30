package cmd

import (
	"context"
	"encoding/json"
	"fmt"
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

// smellRegistry holds the built-in rules.
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
		Description: "Constructs whose tokens (any alias — bare 'Foo' or qualified 'pkg.Foo') have NO entries in node_refs. Aggregated per construct, not per token: a function with three token aliases where any one is referenced is treated as live. Skip list rejects entry points (main, init), interface methods invoked dynamically (String, Error, Read, Write, ...), and the testing-framework prefixes Test*/Benchmark*/Example*/Fuzz* (the runtime invokes those reflectively). source_id falls back to a child's source_file when the construct dir itself doesn't carry one (the schema engine sets source_file on leaf nodes — `source`, `ast.json`, `doc` — but not on the wrapping construct dir). Generated code files (`*.capnp.go`, `*.pb.go`, `*_generated.go`, `*.gen.go`) are excluded because they intentionally export wide APIs the consumer picks from. False positives still expected for exported API consumed outside the indexed scope.",
		Requires:    []string{"node_defs", "node_refs", "nodes"},
		ScopeColumn: "COALESCE(NULLIF(n.source_file, ''), cs.source_file, '')",
		Query: `
			-- A construct is "alive" if ANY of its token aliases appears
			-- in node_refs. We flag a construct as dead only when every
			-- one of its tokens is unreferenced.
			WITH alive AS (
				SELECT DISTINCT d.node_id
				FROM node_defs d
				JOIN node_refs r ON r.token = d.token
			),
			-- Skip-listed: any token of a construct matches a skip rule.
			-- ANY-MATCH is the right semantic — a function with both
			-- 'TestFoo' and 'pkg.TestFoo' should be skipped on the
			-- testing-framework rule even if only one row triggers.
			skipped AS (
				SELECT DISTINCT node_id FROM node_defs
				WHERE token IN ('main','init','String','Error','Read','Write','Close','Len','Less','Swap','MarshalJSON','UnmarshalJSON','Format','Scan')
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
			WHERE n.id IN (SELECT DISTINCT node_id FROM node_defs)
			  AND n.id NOT IN (SELECT node_id FROM alive)
			  AND n.id NOT IN (SELECT node_id FROM skipped)
			  -- Imports are external references, not defs. Their
			  -- "tokens" (e.g. '"fmt"') never appear in node_refs
			  -- because node_refs tracks function calls, not import
			  -- paths, so every imports/ node looks dead by this
			  -- rule. They aren't — they're how the code references
			  -- external packages. Skip them.
			  AND n.id NOT LIKE '%%/imports/%%'
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
		Description: "Exported Go standalone functions (only constructs under a 'functions/' category) with no Test<Foo> counterpart anywhere in node_defs. Static proxy for test coverage — false positives expected for table-driven tests (one TestFoo covers multiple Foos), test helpers, and exported functions intentionally tested at integration boundaries. Methods, types, constants, variables, and imports are skipped: Go test names use Test<Func> not Test<Receiver>.<Method>, and types/constants don't follow the Test<Name> convention. Excludes Test*/Benchmark*/Example* tokens (they ARE tests) and main/init (entry points). source_id falls back to a child's source_file when the construct dir doesn't carry one. Heuristic is Go-specific — running against a Python or Rust .db will produce mostly noise.",
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
			       0 AS metric
			FROM node_defs d
			JOIN nodes n ON n.id = d.node_id
			LEFT JOIN child_source cs ON cs.node_id = n.id
			LEFT JOIN node_defs t ON t.token = 'Test' || d.token
			WHERE substr(d.token, 1, 1) GLOB '[A-Z]'
			  AND d.token NOT LIKE 'Test%%'
			  AND d.token NOT LIKE 'Benchmark%%'
			  AND d.token NOT LIKE 'Example%%'
			  AND d.token NOT IN ('main','init','String','Error')
			  AND t.token IS NULL
			  -- Restrict to constructs in a 'functions/' category dir.
			  -- 'functions/Foo' (auto-inferred flat shape) and
			  -- 'pkg/functions/Foo' (explicit go-schema package shape)
			  -- both match. Skips methods/, types/, constants/,
			  -- variables/, imports/ — those don't follow the
			  -- TestFoo naming convention.
			  AND (d.node_id LIKE 'functions/%%' OR d.node_id LIKE '%%/functions/%%')
			%s
			ORDER BY source_id, d.token
		`,
	},
	{
		ID:          "duplicate_definitions",
		Description: "Symbols defined under more than one node_id in node_defs — same token, multiple definition sites. Common for genuinely-redundant helpers re-implemented per package; routine for Go interface methods like String/Error/Read/Write where one type per package is expected. The skip list excludes those interface contracts; tune by editing the rule. Metric is the duplicate count, sorted descending. Excludes 'imports/' nodes — they're references TO external packages, not definitions. source_id falls back to a child's source_file when the construct dir doesn't carry one. Cross-language since node_defs is populated by every leyline parser.",
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
				WHERE substr(token, instr(token, '.') + 1) NOT IN (
					'main','init',
					'String','Error','Format','Scan','GoString',
					'Read','Write','Close','Open','Seek','ReadAt','WriteAt','ReadFrom','WriteTo',
					'Len','Less','Swap',
					'MarshalJSON','UnmarshalJSON','MarshalText','UnmarshalText','MarshalBinary','UnmarshalBinary',
					'Marshal','Unmarshal','Reset','Clone','Copy','Equal','Hash','Validate'
				)
				-- Exclude imports/ nodes — those are references TO
				-- external packages (e.g. '"fmt"' appears in many
				-- imports), not duplicate definitions.
				AND node_id NOT LIKE '%%/imports/%%'
				GROUP BY token
				HAVING copies > 1
			) c
			JOIN node_defs d ON d.token = c.token
			JOIN nodes n ON n.id = d.node_id
			LEFT JOIN child_source cs ON cs.node_id = n.id
			-- Apply the same import filter to the join so partial
			-- matches (where one repo's imports overlap with a real
			-- def in another repo) don't leak through.
			WHERE d.node_id NOT LIKE '%%/imports/%%'
			%s
			ORDER BY metric DESC, source_id, d.node_id
		`,
	},
	{
		ID:          "god_file",
		Description: "Source files whose distinct-definition count is at least 10 AND more than 3× the project mean — a fuzzy 'god file' detector that surfaces sprawl relative to the codebase's own distribution rather than a hard line-count cutoff. Metric is the def count, sorted descending. Cross-language since node_defs is populated by every backend. Pairs with long_file (line-count via _ast) for a 'lots of code' vs 'lots of API' split.",
		Requires:    []string{"node_defs", "nodes"},
		ScopeColumn: "pf.file",
		Query: `
			WITH per_file AS (
				SELECT n.source_file AS file, COUNT(DISTINCT d.token) AS n
				FROM node_defs d
				JOIN nodes n ON n.id = d.node_id
				WHERE COALESCE(n.source_file, '') != ''
				GROUP BY n.source_file
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
			%s
			ORDER BY metric DESC, source_id
		`,
	},
	{
		ID:          "fan_out_skew",
		Description: "Constructs whose distinct callee count via node_refs is at least 5 AND more than 3× the project mean — likely god-functions / orchestrators that touch too many neighbors. Metric is the fan-out count, sorted descending. Language-agnostic (every leyline parser populates node_refs by token). Skip-listed: testing-framework prefixes Test*/Benchmark*/Example*/Fuzz* — tests are expected to call many things, no signal there. The 3× threshold and 5-call floor are heuristics; adjust by editing the rule body. Pairs with get_communities for 'this construct sprawls across community boundaries' analysis.",
		Requires:    []string{"node_refs", "nodes"},
		ScopeColumn: "COALESCE(n.source_file, '')",
		Query: `
			WITH fanout AS (
				SELECT node_id AS caller_id, COUNT(DISTINCT token) AS n
				FROM node_refs
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
			return mcp.NewToolResultError(fmt.Sprintf("unknown rule %q. Call this tool with no rule for the registry listing.", ruleID)), nil
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
		s := findings[i].StartByte - padding
		if s < 0 {
			s = 0
		}
		e := findings[i].EndByte + padding
		if e > len(src) {
			e = len(src)
		}
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
