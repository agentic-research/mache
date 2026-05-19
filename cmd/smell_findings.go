package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
