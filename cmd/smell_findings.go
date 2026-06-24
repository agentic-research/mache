package cmd

import (
	"database/sql"
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Release the connection before the enrichment query so we don't hold
	// two open cursors on a single-connection backend.
	_ = rows.Close()
	if err := enrichLocations(qg, out); err != nil {
		return nil, err
	}
	return out, nil
}

// enrichLocChunk bounds the _ast IN(...) lookup batch size, kept well under
// SQLite's default 999 bound-variable limit so a rule returning thousands of
// findings can't blow the query (var for test injection).
var enrichLocChunk = 900

// enrichLocations backfills byte/line/column on findings whose rule left them
// at the zero-location default. The node_defs-based rules (dead_code,
// duplicate_definitions, god_file, fan_out_skew, untested_function) emit
// `0 AS start_byte` in SQL because node_defs/nodes don't carry spans — but the
// span already ships in _ast keyed by node_id (mache-ae54d8). One batched
// lookup patches every still-zero finding. No-op when _ast is absent (the
// standalone tree-sitter backend) — the query errors and findings keep their
// file-level location, an honest degradation. _ast-native rules (long_function,
// cyclomatic, …) already compute a span in SQL and are left untouched.
func enrichLocations(qg refsQuerier, findings []smellFinding) error {
	needsLoc := func(f smellFinding) bool { return f.StartByte == 0 && f.Line <= 1 }
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for i := range findings {
		f := findings[i]
		if f.NodeID == "" || (!needsLoc(f) && f.SourceID != "") {
			continue // already fully located, or no node_id to key on
		}
		if !seen[f.NodeID] {
			seen[f.NodeID] = true
			ids = append(ids, f.NodeID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// _ast is absent on the standalone tree-sitter backend — that's an
	// expected no-op, not an error. Probe once (rather than string-matching a
	// "no such table" failure) so that any error from the chunked lookups below
	// is a genuine query failure we must surface, not silent location loss.
	hasAST, err := tableHasColumn(qg, "_ast", "node_id")
	if err != nil {
		return fmt.Errorf("enrich locations: probe _ast: %w", err)
	}
	if !hasAST {
		return nil
	}

	type loc struct {
		sourceID           string
		startByte, endByte int
		startRow, startCol int
	}
	byNode := make(map[string]loc, len(ids))
	// Chunk the IN(...) list under SQLite's bound-variable limit so a large
	// finding set can't fail the lookup (and silently revert to line:1).
	for start := 0; start < len(ids); start += enrichLocChunk {
		end := min(start+enrichLocChunk, len(ids))
		batch := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		q := "SELECT node_id, source_id, start_byte, end_byte, start_row, start_col FROM _ast WHERE node_id IN (" + placeholders + ")"
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := qg.QueryRefs(q, args...)
		if err != nil {
			// _ast exists (probed above) yet the lookup failed — a real error
			// (e.g. too-many-variables); surface it instead of swallowing.
			return fmt.Errorf("enrich locations: _ast lookup: %w", err)
		}
		for rows.Next() {
			var (
				id, srcID        string
				sb, eb           int
				startRow, startC sql.NullInt64
			)
			if err := rows.Scan(&id, &srcID, &sb, &eb, &startRow, &startC); err != nil {
				_ = rows.Close()
				return fmt.Errorf("enrich locations: scan: %w", err)
			}
			byNode[id] = loc{srcID, sb, eb, int(startRow.Int64), int(startC.Int64)}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("enrich locations: rows: %w", err)
		}
		_ = rows.Close()
	}

	for i := range findings {
		l, ok := byNode[findings[i].NodeID]
		if !ok {
			continue
		}
		// Backfill the span only when the rule left it zero (don't clobber
		// SQL-computed spans from _ast-native rules).
		if needsLoc(findings[i]) {
			findings[i].StartByte = l.startByte
			findings[i].EndByte = l.endByte
			findings[i].Line = l.startRow + 1
			findings[i].Column = l.startCol + 1
		}
		// Backfill the file when the rule's source_file fallback was empty.
		if findings[i].SourceID == "" {
			findings[i].SourceID = l.sourceID
		}
	}
	return nil
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
