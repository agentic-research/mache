package smells

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
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

// baselineVersion is the current schema.
//
//	v1: keyed (rule_id, source_id) — the file PATH.
//	v2: keyed (rule_id, node_hash) when the producer supplies a content
//	    address, falling back to the path when it does not.
//
// v1 files still load and still gate, path-keyed exactly as before; a
// regeneration produces v2. That matters because the whole point of v2 is to
// survive moves, and a migration that made every existing entry miss would
// fail the gate on the first refactor after upgrading — the very thing this
// is fixing (mache-dd45a3).
const baselineVersion = 2

type baselineEntry struct {
	RuleID string `json:"rule_id"`
	// SourceID is where the finding lived when the baseline was written. On
	// v2 it is informational — it keeps the file human-reviewable — EXCEPT
	// for entries with no NodeHash, where it is still the key.
	SourceID string `json:"source_id"`
	// NodeHash is the content address this entry is keyed on. Empty means the
	// producer offered none, so the entry stays path-keyed.
	NodeHash string `json:"node_hash,omitempty"`
	Count    int    `json:"count"`
}

// ratchetKey is the identity a grandfathered count is filed under.
//
// Prefixed so the two key spaces can never collide: a content address and a
// file path are different KINDS of claim, and silently letting one satisfy
// the other is how a moved file would spend an allowance twice.
func ratchetKey(ruleID, nodeHash, sourceID string, version int) [2]string {
	if version >= 2 && nodeHash != "" {
		return [2]string{ruleID, "h:" + nodeHash}
	}
	return [2]string{ruleID, "p:" + sourceID}
}

// computeBaseline counts findings per ratchet key. The output is sorted
// deterministically so a committed baseline file doesn't churn on regeneration.
//
// A content-addressed key POOLS byte-identical debt across files: at the time
// of writing, 21 of 624 hashed keys in this repo's own baseline span more than
// one file — `time.Sleep(50 * time.Millisecond)` is the same content wherever
// it appears, and a content address cannot tell two copies apart. That is the
// same property that makes the key survive a move, so it is not separable from
// the fix.
//
// The pooled allowance stays honest: N identical instances, allowance N.
// Adding an (N+1)th still fails; relocating one is net-zero, which is exactly
// what a move-invariant ratchet should permit.
//
// SourceID is then only a human pointer to ONE of the files involved, so it is
// chosen as the smallest rather than the first encountered. First-encountered
// would be scan-order dependent, and a committed file that reshuffles between
// identical runs is a diff nobody can review.
func computeBaseline(findings []smellFinding) smellBaseline {
	type ident struct{ ruleID, nodeHash, sourceID string }
	counts := map[[2]string]int{}
	idents := map[[2]string]ident{}
	for _, f := range findings {
		k := ratchetKey(f.RuleID, f.NodeHash, f.SourceID, baselineVersion)
		counts[k]++
		if prev, seen := idents[k]; !seen || f.SourceID < prev.sourceID {
			idents[k] = ident{f.RuleID, f.NodeHash, f.SourceID}
		}
	}
	out := smellBaseline{Version: baselineVersion, Counts: make([]baselineEntry, 0, len(counts))}
	for k, n := range counts {
		id := idents[k]
		out.Counts = append(out.Counts, baselineEntry{
			RuleID: id.ruleID, SourceID: id.sourceID, NodeHash: id.nodeHash, Count: n,
		})
	}
	sortByTriple(out.Counts, func(x baselineEntry) (string, string, string) {
		return x.RuleID, x.SourceID, x.NodeHash
	})
	return out
}

// lookup returns the grandfathered count for a ratchet key, or 0 if absent.
//
// Absent means NOT grandfathered, which is what closes the laundering hole:
// an entry whose content address no longer exists in the tree matches nothing,
// so a vacated file cannot keep spending an allowance it no longer needs.
func (b smellBaseline) lookup(key [2]string) int {
	for _, e := range b.Counts {
		if ratchetKey(e.RuleID, e.NodeHash, e.SourceID, b.Version) == key {
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
		// Group by the SAME key the baseline is filed under, so a moved
		// finding lands in its grandfathered group rather than a new one.
		k := ratchetKey(f.RuleID, f.NodeHash, f.SourceID, base.Version)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], f)
	}
	slices.SortFunc(order, func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})

	var debt []smellFinding
	for _, k := range order {
		g := groups[k]
		allowed := base.lookup(k)
		if len(g) <= allowed {
			continue
		}
		sortByTriple(g, func(x smellFinding) (int, int, int) {
			return x.Line, x.Column, x.StartByte
		})
		debt = append(debt, g[allowed:]...)
	}
	return debt
}

// sortByTriple sorts s by the cascading comparison of a three-part key. One
// comparator serves both orderings below, so they cannot drift apart — and
// two hand-rolled sort.Slice bodies that differ only in field names are
// exactly the structural clone duplicate_code is built to catch.
func sortByTriple[T any, K cmp.Ordered](s []T, key func(T) (K, K, K)) {
	slices.SortFunc(s, func(a, b T) int {
		a1, a2, a3 := key(a)
		b1, b2, b3 := key(b)
		return cmp.Or(cmp.Compare(a1, b1), cmp.Compare(a2, b2), cmp.Compare(a3, b3))
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
