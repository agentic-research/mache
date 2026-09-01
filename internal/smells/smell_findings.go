package smells

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/graph"
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
	// NodeHash is the finding's CONTENT address (hex BLAKE3 of the merkle
	// subtree), empty on producers that write no node_hash. It is what makes
	// the ratchet survive a refactor: the same code carries the same hash at
	// whatever path it lives (verified — an identical function at a/thing.go
	// and b/deep/moved.go hashes identically), so moved debt stays
	// grandfathered and a vacated path stops spending an allowance
	// (mache-dd45a3).
	NodeHash string `json:"node_hash,omitempty"`
}

// ensureSmellQueryContext installs the canonical views/tables (v_defs,
// v_refs, v_test_nodes, v_doc_refs) and loads the capnp binding sidecar for
// qg's connection. v_test_nodes and v_doc_refs are materialized TEMP TABLEs,
// not views, specifically so their cost is paid once per connection rather
// than once per rule (see ensureTestNodesView's comment) — but that only
// holds if the CALLER invokes this once and then runs every rule with
// runSmellRuleQuery. A caller that runs N rules against the same qg in one
// process (find-smells --rule '*', the MCP digest) MUST call this once
// before the loop; calling RunSmellRule per rule instead silently
// re-materializes both tables N times (measured: ~9s/rule on a mid-size
// Rust repo's leyline .db, dominating the whole invocation — mache-808b0b).
func ensureSmellQueryContext(qg graph.RefsQuerier) error {
	if err := EnsureCanonicalViews(qg); err != nil {
		return err
	}
	// Capnp readthrough (mache-190508 step 3): if this querier knows
	// its dbPath AND a sibling .bindings.capnp event log exists, load
	// its records into the per-connection _capnp_binding_refs TEMP
	// table. v_refs is already configured to UNION over that table
	// (in EnsureCanonicalViews); empty table → no extra rows.
	if dp, ok := qg.(graph.DBPathProvider); ok {
		if err := LoadCapnpBindings(qg, dp.DBPath()); err != nil {
			return err
		}
	}
	return nil
}

// RunSmellRule executes the rule's SQL, optionally scoped to one source.
// Single-rule callers (the MCP find_smells tool, one rule per request) use
// this directly. Callers that run multiple rules against the same qg in one
// process must call ensureSmellQueryContext once before the loop and use
// runSmellRuleQuery per rule instead — see ensureSmellQueryContext.
func RunSmellRule(qg graph.RefsQuerier, rule *SmellRule, sourceID string, limit int) ([]smellFinding, error) {
	if err := ensureSmellQueryContext(qg); err != nil {
		return nil, err
	}
	return runSmellRuleQuery(qg, rule, sourceID, limit)
}

// runSmellRuleQuery executes rule's SQL assuming ensureSmellQueryContext has
// already run on qg's connection. Extracted out of RunSmellRule so
// multi-rule callers pay the view/table setup cost once instead of once per
// rule.
func runSmellRuleQuery(qg graph.RefsQuerier, rule *SmellRule, sourceID string, limit int) ([]smellFinding, error) {
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
	// Content addresses first: the ratchet keys on them, so a finding without
	// one silently degrades to path keying (mache-dd45a3).
	if err := enrichNodeHashes(qg, out); err != nil {
		return nil, err
	}
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
// enrichNodeHashes fills in each finding's content address from `_ast`.
//
// Only CONSTRUCT-level findings get one. File-level rules (god_file,
// long_file) emit an empty node_id, and they are deliberately left
// path-keyed.
//
// The tempting move is to key them on the file's own `_ast` row, which does
// exist. Measured, that is a trap: a file's merkle hash covers its whole
// content, so ANY edit changes it — verified, one appended function took
// 6e1abfb0… to 15986e62…. A god_file entry keyed that way would stop matching
// the moment anyone touched the file, so the next PR editing any of the
// thirteen current god files would fail the gate on debt it did not add.
// That is a worse failure than the one this fixes, and in the opposite
// direction.
//
// For "this FILE is too big", the path IS the identity. Move-invariance is
// what construct-level findings need, because a construct is what gets moved.
//
// A producer with no `_ast` leaves every hash empty, and the ratchet falls
// back to path keying for those findings too. That degradation is deliberate
// and documented rather than an error: a standalone backend genuinely has no
// content address to offer.
func enrichNodeHashes(qg graph.RefsQuerier, findings []smellFinding) error {
	ids := make([]string, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		if f.NodeID == "" || seen[f.NodeID] {
			continue // file-level finding, or already queued
		}
		seen[f.NodeID] = true
		ids = append(ids, f.NodeID)
	}
	if len(ids) == 0 {
		return nil
	}
	hasHash, err := TableHasColumn(qg, "_ast", "node_hash")
	if err != nil {
		return fmt.Errorf("enrich node hashes: probe _ast.node_hash: %w", err)
	}
	if !hasHash {
		return nil // standalone producer: path keying, by design
	}

	byNode := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += enrichLocChunk {
		end := min(start+enrichLocChunk, len(ids))
		batch := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		q := "SELECT node_id, hex(node_hash) FROM _ast WHERE node_id IN (" + placeholders + ")"
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := qg.QueryRefs(q, args...)
		if err != nil {
			return fmt.Errorf("enrich node hashes: _ast lookup: %w", err)
		}
		for rows.Next() {
			var id string
			var hash sql.NullString
			if err := rows.Scan(&id, &hash); err != nil {
				_ = rows.Close()
				return fmt.Errorf("enrich node hashes: scan: %w", err)
			}
			if hash.Valid && hash.String != "" {
				// Normalize case: SQL hex() returns UPPERCASE while the
				// incremental memo carries lowercase. Identical content
				// reaching the ratchet under two spellings would key as two
				// different findings — caught by the memo-vs-full-scan
				// byte-equality test, which is exactly what it is for.
				byNode[id] = strings.ToLower(hash.String)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("enrich node hashes: rows: %w", err)
		}
		_ = rows.Close()
	}

	for i := range findings {
		if findings[i].NodeID == "" {
			continue
		}
		if h, ok := byNode[findings[i].NodeID]; ok {
			findings[i].NodeHash = h
		}
	}
	return nil
}

func enrichLocations(qg graph.RefsQuerier, findings []smellFinding) error {
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
	hasAST, err := TableHasColumn(qg, "_ast", "node_id")
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
func populateSnippets(qg graph.RefsQuerier, findings []smellFinding) {
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

// smellResponse is the on-the-wire envelope for a single rule's run. Both
// consumers of the smell engine emit it: the find_smells MCP tool and the
// find-smells CLI's json/md formats.
//
// It lives here, constructed by one function, because the two paths had
// already drifted while both "looked right" in isolation. They are supposed to
// be the same rules over the same .db producing the same answer — that is what
// makes a committed baseline generated by the CLI meaningful to an agent
// calling the MCP tool.
type smellResponse struct {
	Rule  string `json:"rule"`
	Total int    `json:"total"`
	// Truncated reports that Total is a floor rather than a count: the query
	// returned as many rows as the cap allowed, so an unknown number more may
	// exist.
	//
	// Without it the envelope is indistinguishable between "this repo has 200
	// findings" and "here are the first 200 of some larger number" — and the
	// second is the common case on anything big. Measured on mache's own
	// 862-file tree at the default cap of 200: cyclomatic_complexity has 3,980
	// findings, magic_int_in_comparison 1,430, duplicate_code 340. Three of
	// fourteen rules, under-reported by up to 20x, with nothing in the payload
	// saying so (mache-2b1ea7).
	//
	// Omitted when false so a clean, complete result keeps its existing shape.
	Truncated bool           `json:"truncated,omitempty"`
	Findings  []smellFinding `json:"findings"`
}

// newSmellResponse builds the envelope, normalizing a nil finding set to an
// empty slice.
//
// That normalization is contract, not cosmetics: a nil slice marshals to
// `null`, which forces every JSON consumer to special-case the clean-gate
// case — the common one — and diverges from --format=sarif, which always
// emits arrays. The CLI did this and the MCP handler did not, so the identical
// clean run serialized as `"findings": []` from one path and `"findings":
// null` from the other.
// limit is the cap the query ran under, used to detect truncation. Pass
// noSmellLimit when the caller did not impose one.
//
// The detection is len(findings) >= limit, which is deliberately CONSERVATIVE:
// a rule with exactly limit findings is reported as possibly truncated when it
// is not. That direction is the safe one — over-warning costs a reader one
// re-run with a higher cap, while under-warning is the silence this exists to
// end. Getting it exact would mean asking the database for a count, which for
// these analytic queries costs as much as producing the rows.
//
// `>=` rather than `==` because a caller handing over MORE rows than the cap is
// a caller bug, and answering "complete" for it would be the worst outcome
// available. SQL LIMIT makes that unreachable today; the predicate does not
// depend on it staying unreachable.
func newSmellResponse(ruleID string, findings []smellFinding, limit int) smellResponse {
	if findings == nil {
		findings = []smellFinding{}
	}
	return smellResponse{
		Rule:      ruleID,
		Total:     len(findings),
		Truncated: limit > 0 && len(findings) >= limit,
		Findings:  findings,
	}
}

// noSmellLimit marks a call that ran without a cap, so truncation is impossible.
const noSmellLimit = 0

func jsonOrPanic(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": "marshal: %v"}`, err)
	}
	return string(b)
}
