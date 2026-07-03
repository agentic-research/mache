package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// relativizeFindings trims root (plus a trailing separator) from each finding's
// SourceID — mache build records absolute paths, so a committed baseline must
// be relativized to stay portable across machines and CI. No-op when root == "".
func relativizeFindings(findings []smellFinding, root string) []smellFinding {
	if root == "" {
		return findings
	}
	prefix := strings.TrimRight(root, string(os.PathSeparator)) + string(os.PathSeparator)
	out := make([]smellFinding, len(findings))
	for i, f := range findings {
		f.SourceID = strings.TrimPrefix(f.SourceID, prefix)
		out[i] = f
	}
	return out
}

// smellBaseline is the grandfathered count of findings per (rule_id, source_id)
// — a count-based ratchet that generalizes rosary's god-files-vs-origin/main:
// existing debt is allowed; the gate fails only on findings ABOVE the baseline
// count, and the baseline may shrink freely (improvement). Count-based rather
// than line-based so it's stable across incidental line shifts (adding a line
// above a finding must not "move" it into new-debt).
//
// This is the enforced ratchet the observational smell-debt-baseline doc was
// missing; `task smells` (W5) wraps `mache find-smells` + this to become the
// local-first gate, and the find-smells GHA (W6) wraps `task smells`.
// Bead mache-491b9f (analysis-substrate/W5).
type smellBaseline struct {
	Version int             `json:"version"`
	Counts  []baselineEntry `json:"counts"`
}

type baselineEntry struct {
	RuleID   string `json:"rule_id"`
	SourceID string `json:"source_id"`
	Count    int    `json:"count"`
}

// computeBaseline counts findings per (rule, file). The output is sorted
// deterministically so a committed baseline file doesn't churn on regeneration.
func computeBaseline(findings []smellFinding) smellBaseline {
	counts := map[[2]string]int{}
	for _, f := range findings {
		counts[[2]string{f.RuleID, f.SourceID}]++
	}
	out := smellBaseline{Version: 1, Counts: make([]baselineEntry, 0, len(counts))}
	for k, n := range counts {
		out.Counts = append(out.Counts, baselineEntry{RuleID: k[0], SourceID: k[1], Count: n})
	}
	sortEntries(out.Counts)
	return out
}

// lookup returns the grandfathered count for (rule, file), or 0 if absent.
func (b smellBaseline) lookup(ruleID, sourceID string) int {
	for _, e := range b.Counts {
		if e.RuleID == ruleID && e.SourceID == sourceID {
			return e.Count
		}
	}
	return 0
}

// newDebt returns the findings that exceed the baseline count for their
// (rule, file) — exactly what a ratcheted gate fails on. Within each group the
// first `allowed` findings (sorted by position) are grandfathered and the rest
// are returned. Deterministic in group order and within-group order.
func newDebt(current []smellFinding, base smellBaseline) []smellFinding {
	groups := map[[2]string][]smellFinding{}
	var order [][2]string
	for _, f := range current {
		k := [2]string{f.RuleID, f.SourceID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], f)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i][0] != order[j][0] {
			return order[i][0] < order[j][0]
		}
		return order[i][1] < order[j][1]
	})

	var debt []smellFinding
	for _, k := range order {
		g := groups[k]
		allowed := base.lookup(k[0], k[1])
		if len(g) <= allowed {
			continue
		}
		sortFindingsByPosition(g)
		debt = append(debt, g[allowed:]...)
	}
	return debt
}

func sortEntries(e []baselineEntry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].RuleID != e[j].RuleID {
			return e[i].RuleID < e[j].RuleID
		}
		return e[i].SourceID < e[j].SourceID
	})
}

func sortFindingsByPosition(f []smellFinding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Line != f[j].Line {
			return f[i].Line < f[j].Line
		}
		if f[i].Column != f[j].Column {
			return f[i].Column < f[j].Column
		}
		return f[i].StartByte < f[j].StartByte
	})
}

// loadBaseline reads a committed smellBaseline JSON file.
func loadBaseline(path string) (smellBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return smellBaseline{}, err
	}
	var b smellBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return smellBaseline{}, fmt.Errorf("parse baseline: %w", err)
	}
	return b, nil
}

// writeBaseline writes a deterministic (sorted) smellBaseline JSON file.
func writeBaseline(path string, b smellBaseline) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// allFindings flattens the per-rule results into a single finding slice.
func allFindings(results []ruleRunResult) []smellFinding {
	var out []smellFinding
	for _, r := range results {
		out = append(out, r.findings...)
	}
	return out
}

// renderNewDebt prints the findings that exceed the baseline — the actionable
// output of a ratcheted gate (one line per finding + a count header).
func renderNewDebt(w io.Writer, debt []smellFinding) {
	_, _ = fmt.Fprintf(w, "smell ratchet: %d NEW finding(s) above baseline:\n", len(debt))
	for _, f := range debt {
		loc := f.SourceID
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.SourceID, f.Line)
		}
		_, _ = fmt.Fprintf(w, "  [%s] %s\n", f.RuleID, loc)
	}
}
