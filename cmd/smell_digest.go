package cmd

import (
	"sort"
)

// digestScanCap bounds how many findings each rule contributes to a digest
// scan. Far above any rule's realistic finding count on a single repo, so the
// count is exact in practice; when a rule does hit the cap the digest marks it
// truncated rather than silently undercounting (the exact failure mode the
// tiered response exists to avoid).
var digestScanCap = 10000

// ruleDigest is the L1 summary for one rule: how many findings, a handful of
// the worst (highest-metric) exemplars with real locations, and whether the
// scan was truncated at the cap.
type ruleDigest struct {
	Rule      string         `json:"rule"`
	Count     int            `json:"count"`
	Truncated bool           `json:"truncated,omitempty"`
	Worst     []smellFinding `json:"worst,omitempty"`
}

// fileCount is one row of the by-file rollup.
type fileCount struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

// smellDigest is the L1 "shape without weight" response: total + per-rule
// counts + per-file rollup + a few worst exemplars per rule, instead of a flat
// (capped) dump of every finding. Drill down to L2 with `rule=<id>` (optionally
// `source_id=<file>`), which returns the full findings for that slice with the
// real _ast locations enrichLocations backfills.
type smellDigest struct {
	Total     int          `json:"total"`
	Rules     int          `json:"rules_scanned"`
	Skipped   []string     `json:"rules_skipped,omitempty"` // tables not present on this backend
	ByRule    []ruleDigest `json:"by_rule"`
	ByFileTop []fileCount  `json:"by_file_top,omitempty"`
	DrillHelp string       `json:"drill_help"`
}

// buildSmellDigest runs every rule in the given set whose Requires tables are
// present and rolls the findings up into an L1 digest. Rules whose tables are
// absent are reported in Skipped rather than erroring — the same
// graceful-degradation the per-rule pre-flight uses. The rules argument is the
// active set (built-ins + external) resolved by the caller, so externally-loaded
// rules participate in the digest.
func buildSmellDigest(qg refsQuerier, rules []SmellRule, worstN, fileTopN int) (smellDigest, error) {
	d := smellDigest{
		DrillHelp: "drill down with rule=<id> (+ optional source_id=<file>) for the full findings of one rule, each with its real file:line from _ast.",
	}
	byFile := map[string]int{}

	for i := range rules {
		rule := &rules[i]
		missing, err := missingTables(qg, rule.Requires)
		if err != nil {
			return smellDigest{}, err
		}
		if len(missing) > 0 {
			d.Skipped = append(d.Skipped, rule.ID)
			continue
		}
		d.Rules++

		findings, err := runSmellRule(qg, rule, "", digestScanCap)
		if err != nil {
			return smellDigest{}, err
		}
		// Capture whether the SCAN hit the cap before any metric filtering —
		// otherwise a rule whose default threshold drops the result below the
		// cap would mis-report Truncated=false while the underlying scan was
		// actually capped (and Count understated).
		scannedAtCap := len(findings) >= digestScanCap
		// Apply the rule's default metric threshold so the digest counts match
		// what a bare `rule=<id>` call would surface (which also applies it).
		if rule.DefaultMinMetric > 0 {
			kept := findings[:0]
			for _, f := range findings {
				if f.Metric >= rule.DefaultMinMetric {
					kept = append(kept, f)
				}
			}
			findings = kept
		}
		if len(findings) == 0 {
			continue
		}

		// Worst-first by metric (ties broken by file then line for stability).
		sort.Slice(findings, func(a, b int) bool {
			if findings[a].Metric != findings[b].Metric {
				return findings[a].Metric > findings[b].Metric
			}
			if findings[a].SourceID != findings[b].SourceID {
				return findings[a].SourceID < findings[b].SourceID
			}
			return findings[a].Line < findings[b].Line
		})

		rd := ruleDigest{
			Rule:      rule.ID,
			Count:     len(findings),
			Truncated: scannedAtCap,
		}
		for j := 0; j < worstN && j < len(findings); j++ {
			rd.Worst = append(rd.Worst, findings[j])
		}
		d.ByRule = append(d.ByRule, rd)
		d.Total += len(findings)
		for _, f := range findings {
			if f.SourceID != "" {
				byFile[f.SourceID]++
			}
		}
	}

	// Stable per-rule order: worst (highest count) first.
	sort.Slice(d.ByRule, func(a, b int) bool {
		if d.ByRule[a].Count != d.ByRule[b].Count {
			return d.ByRule[a].Count > d.ByRule[b].Count
		}
		return d.ByRule[a].Rule < d.ByRule[b].Rule
	})

	d.ByFileTop = topFiles(byFile, fileTopN)
	return d, nil
}

// topFiles returns the n files with the most findings, count-descending then
// path-ascending for stability.
func topFiles(byFile map[string]int, n int) []fileCount {
	out := make([]fileCount, 0, len(byFile))
	for f, c := range byFile {
		out = append(out, fileCount{File: f, Count: c})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		return out[a].File < out[b].File
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
