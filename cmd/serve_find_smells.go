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
// The query MUST select exactly six columns:
//
//	source_id, node_id, start_byte, end_byte, start_row, start_col
//
// in that order. The handler shapes them into a uniform response with
// 1-based line/column and an optional snippet.
//
// `ScopeColumn` is the SQL expression the handler will compare to
// source_id when the caller passes `source_id` to find_smells (e.g.
// "lit.source_id" for AST-walking rules; "n.source_file" for rules
// that join via nodes). The query template MUST contain a single
// `%s` placeholder where the scope clause should be spliced in;
// rules unconcerned with scoping can keep the placeholder empty by
// leaving ScopeColumn blank.
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
}

// smellRegistry holds the built-in rules.
var smellRegistry = []SmellRule{
	{
		ID:          "magic_int_in_comparison",
		Languages:   []string{"go"},
		Description: "Go binary expressions where an int_literal appears as a direct operand. Each match is a candidate magic constant — replace with a named const if the value carries domain meaning.",
		ScopeColumn: "lit.source_id",
		Query: `
			SELECT lit.source_id, lit.node_id, lit.start_byte, lit.end_byte, lit.start_row, lit.start_col
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
		Description: "Symbols defined in node_defs that have no entries in node_refs — nothing in the indexed graph references them. False positives expected for entry points (main, init), interface methods invoked dynamically (String, Error), and exported API consumed outside the indexed scope. The skip list at the top of the rule excludes the most common offenders; tune by editing the rule.",
		ScopeColumn: "COALESCE(n.source_file, '')",
		Query: `
			SELECT COALESCE(n.source_file, '') AS source_id,
			       defs.node_id,
			       0  AS start_byte,
			       0  AS end_byte,
			       0  AS start_row,
			       0  AS start_col
			FROM node_defs defs
			JOIN nodes n ON n.id = defs.node_id
			LEFT JOIN node_refs refs ON refs.token = defs.token
			WHERE refs.token IS NULL
			  AND defs.token NOT IN ('main','init','String','Error','Read','Write','Close','Len','Less','Swap','MarshalJSON','UnmarshalJSON','Format','Scan')
			%s
			ORDER BY COALESCE(n.source_file, ''), defs.node_id
		`,
	},
}

// smellFinding is one row of a smell scan. Byte ranges and (1-based)
// line/column let editors jump straight to the offending span.
type smellFinding struct {
	RuleID    string `json:"rule_id"`
	SourceID  string `json:"source_id"`
	NodeID    string `json:"node_id,omitempty"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Line      int    `json:"line"`              // 1-based
	Column    int    `json:"column"`            // 1-based
	Snippet   string `json:"snippet,omitempty"` // up to ~80 chars of source
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
			return mcp.NewToolResultError(fmt.Sprintf("unknown rule %q. Call this tool with no rule for the registry listing.", ruleID)), nil
		}

		// Need a *sql.DB to run the rule. Today we hand-shake via the
		// refsQuerier interface that other handlers already use.
		qg, ok := g.(refsQuerier)
		if !ok {
			return mcp.NewToolResultError("the active graph backend doesn't expose a SQL handle; find_smells requires a leyline-parsed .db"), nil
		}

		findings, err := runSmellRule(qg, rule, sourceID, limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("rule %q: %v", ruleID, err)), nil
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
	}
	out := make([]ruleSummary, 0, len(smellRegistry))
	for _, r := range smellRegistry {
		out = append(out, ruleSummary{
			ID:          r.ID,
			Languages:   r.Languages,
			Description: r.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return struct {
		Help  string        `json:"help"`
		Rules []ruleSummary `json:"rules"`
	}{
		Help:  "find_smells runs structural pattern queries against the _ast table. Pass `rule` to scan; omit it (this response) to list available rules. Optional `source_id` filters to one parsed file; `limit` caps results (default 200).",
		Rules: out,
	}
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
		)
		if err := rows.Scan(&src, &nodeID, &startByte, &endByte, &startRow, &startCol); err != nil {
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
