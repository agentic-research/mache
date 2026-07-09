package cmd

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// mache-238673 phase-1 — content-addressed (node_hash-memoized) evaluation of
// cyclomatic_complexity, the merkle cash-out that content-addressing was built
// to enable. Rules currently full-scan every run; this path computes each
// distinct function subtree's metric ONCE, keyed by its merkle node_hash, and
// fans the result out to every occurrence.
//
// Correctness is sound by construction: cyclomatic_complexity is a PURE-SUBTREE
// rule (the metric counts a function's own control-flow descendants), so a
// BLAKE3 content address IS the complete dependency set — same node_hash ⟹ same
// subtree ⟹ same metric. There is no hand-rolled dependency graph to get wrong.
// The memo caches ONLY the metric (an int64); every occurrence's SPAN is
// re-read from _ast each scan, so a function that moved but did not change
// keeps its new location. See the differential oracle in
// smell_findings_incremental_test.go — it asserts byte-identical parity with
// the full scan, so any invalidation bug fails the test rather than passing
// silently.
//
// Phase-1 is deliberately narrow: cyclomatic_complexity only, in-process memo,
// NO sheaf dependency. A long-lived caller (e.g. a future sheaf-subscribe
// daemon) holds the memo across scans so an unchanged or duplicated tree is a
// cache hit — the sub-linear re-run win.

// cyclomaticBranchKinds is the set of control-flow AST node kinds counted for
// cyclomatic complexity. It MUST match cmd/rules/cyclomatic_complexity.json;
// the differential oracle (byte-identical vs the rule's own SQL) is the guard
// against drift.
var cyclomaticBranchKinds = []string{
	"if_statement", "for_statement", "case_clause",
	"expression_case", "type_case", "communication_case", "default_case",
}

const cyclomaticRuleID = "cyclomatic_complexity"

// smellMemo memoizes pure-subtree rule metrics keyed by merkle node_hash (hex).
// It is safe to reuse across scans: a hash present in the map was computed from
// content that (by content-addressing) cannot have changed without changing the
// hash. hits/misses are per-process counters the tests and bench read to prove
// the cache is actually load-bearing (a memo that never caches would show zero
// hits and pass correctness trivially — the oracle asserts hits > 0).
type smellMemo struct {
	metric map[string]int64 // node_hash hex → cyclomatic metric
	hits   int              // occurrences served from the memo without recompute
	misses int              // distinct subtrees computed this process (cache misses)
}

func newSmellMemo() *smellMemo {
	return &smellMemo{metric: make(map[string]int64)}
}

// astFunc is one function occurrence read from _ast, before its metric is
// resolved.
type astFunc struct {
	sourceID  string
	nodeID    string
	hashHex   string // "" when the producer wrote no node_hash (standalone db)
	startByte int
	endByte   int
	startRow  int
	startCol  int
}

// runCyclomaticComplexityMemo evaluates cyclomatic_complexity using memo. Its
// output is byte-identical to runSmellRule(qg, cyclomaticRule, sourceID, limit):
// the same findings in the same order (metric DESC, source_id, start_byte),
// truncated to limit. Functions whose node_hash is already memoized skip the
// branch-count join; functions with no node_hash are always computed (never
// cached) so correctness never depends on the producer emitting a hash.
func runCyclomaticComplexityMemo(qg refsQuerier, sourceID string, limit int, memo *smellMemo) ([]smellFinding, error) {
	funcs, err := scanASTFunctions(qg, sourceID)
	if err != nil {
		return nil, err
	}

	// Decide, per occurrence, whether the metric is already known. Schedule at
	// most one compute per distinct key: a non-empty node_hash keys the shared
	// memo; a hashless occurrence keys only itself (no dedup possible).
	seenThisScan := make(map[string]bool) // node_hash hexes scheduled/available this scan
	toCompute := make([]astFunc, 0)       // representatives needing the branch-count join
	local := make(map[string]int64)       // node_id → metric, for hashless occurrences
	for _, f := range funcs {
		if f.hashHex != "" {
			if _, ok := memo.metric[f.hashHex]; ok {
				memo.hits++
				continue
			}
			if seenThisScan[f.hashHex] {
				memo.hits++ // duplicate subtree this scan; the representative fills it
				continue
			}
			seenThisScan[f.hashHex] = true
			toCompute = append(toCompute, f)
			memo.misses++
			continue
		}
		// No node_hash: cannot dedup or cache — compute this occurrence.
		toCompute = append(toCompute, f)
		memo.misses++
	}

	computed, err := computeCyclomaticMetrics(qg, toCompute)
	if err != nil {
		return nil, err
	}
	for _, f := range toCompute {
		m := computed[f.nodeID]
		if f.hashHex != "" {
			memo.metric[f.hashHex] = m
		} else {
			local[f.nodeID] = m
		}
	}

	// Build findings, resolving each occurrence's metric from the memo (hashed)
	// or the per-occurrence local map (hashless).
	out := make([]smellFinding, 0, len(funcs))
	for _, f := range funcs {
		var m int64
		if f.hashHex != "" {
			m = memo.metric[f.hashHex]
		} else {
			m = local[f.nodeID]
		}
		out = append(out, smellFinding{
			RuleID:    cyclomaticRuleID,
			SourceID:  f.sourceID,
			NodeID:    f.nodeID,
			StartByte: f.startByte,
			EndByte:   f.endByte,
			Line:      f.startRow + 1,
			Column:    f.startCol + 1,
			Metric:    m,
		})
	}

	// Match the rule's ORDER BY metric DESC, source_id, start_byte. (source_id,
	// start_byte) is unique per occurrence, so the order is total and
	// deterministic — a prerequisite for byte-identical parity.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Metric != out[j].Metric {
			return out[i].Metric > out[j].Metric
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].StartByte < out[j].StartByte
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}

	// Parity with runSmellRule (no-op for cyclomatic: spans are already set, so
	// enrichLocations finds nothing to backfill).
	if err := enrichLocations(qg, out); err != nil {
		return nil, err
	}
	return out, nil
}

// scanASTFunctions reads every function/method occurrence from _ast, with its
// node_hash (when the column exists) and span. Optionally scoped to one source.
func scanASTFunctions(qg refsQuerier, sourceID string) ([]astFunc, error) {
	hasHash, err := tableHasColumn(qg, "_ast", "node_hash")
	if err != nil {
		return nil, fmt.Errorf("probe _ast.node_hash: %w", err)
	}
	hashExpr := "NULL"
	if hasHash {
		hashExpr = "node_hash"
	}
	q := "SELECT source_id, node_id, " + hashExpr +
		", start_byte, end_byte, start_row, start_col FROM _ast" +
		" WHERE node_kind IN ('function_declaration','method_declaration')"
	args := []any{}
	if sourceID != "" {
		q += " AND source_id = ?"
		args = append(args, sourceID)
	}

	rows, err := qg.QueryRefs(q, args...)
	if err != nil {
		return nil, fmt.Errorf("scan _ast functions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []astFunc
	for rows.Next() {
		var (
			f    astFunc
			hash []byte
		)
		if err := rows.Scan(&f.sourceID, &f.nodeID, &hash, &f.startByte, &f.endByte, &f.startRow, &f.startCol); err != nil {
			return nil, fmt.Errorf("scan _ast function: %w", err)
		}
		if len(hash) > 0 {
			f.hashHex = hex.EncodeToString(hash)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// computeCyclomaticMetrics runs the branch-count join for the given function
// occurrences (representatives of distinct subtrees) and returns node_id →
// metric. Empty input is a no-op — the whole point of the memo is that a warm
// re-run schedules nothing.
func computeCyclomaticMetrics(qg refsQuerier, funcs []astFunc) (map[string]int64, error) {
	out := make(map[string]int64, len(funcs))
	if len(funcs) == 0 {
		return out, nil
	}
	kindList := "'" + strings.Join(cyclomaticBranchKinds, "','") + "'"
	ids := make([]string, len(funcs))
	for i, f := range funcs {
		ids[i] = f.nodeID
	}
	// Chunk the IN(...) list under SQLite's bound-variable limit.
	for start := 0; start < len(ids); start += enrichLocChunk {
		end := min(start+enrichLocChunk, len(ids))
		batch := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		q := "SELECT fn.node_id, COUNT(branch.node_id) AS metric FROM _ast fn" +
			" LEFT JOIN _ast branch ON branch.source_id = fn.source_id" +
			" AND branch.node_id LIKE fn.node_id || '/%'" +
			" AND branch.node_kind IN (" + kindList + ")" +
			" WHERE fn.node_id IN (" + placeholders + ")" +
			" GROUP BY fn.node_id"
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := qg.QueryRefs(q, args...)
		if err != nil {
			return nil, fmt.Errorf("compute cyclomatic metrics: %w", err)
		}
		for rows.Next() {
			var (
				id     string
				metric int64
			)
			if err := rows.Scan(&id, &metric); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan cyclomatic metric: %w", err)
			}
			out[id] = metric
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
