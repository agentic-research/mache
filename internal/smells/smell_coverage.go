package smells

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// skippedRule is a rule that could not run because this backend lacks the
// tables it needs.
//
// Skipping is the right behaviour — a rule that needs `_ast` cannot invent it,
// and aborting the whole run because one rule is unsupported would make the
// gate useless on a standalone .db. What was wrong is that the skip left no
// trace anywhere a consumer looks: a rule that no-ops reports zero findings,
// which is byte-identical to a rule that ran and found nothing. The ratchet
// then reports "0 NEW" and the gate goes green having assessed less than it
// claims. That is a disabled linter, arrived at politely (mache-ddf14b).
type skippedRule struct {
	ID      string
	Missing []string
}

// coverageRegression returns the rules this run skipped that the baseline
// records as having RUN when it was written.
//
// This is the evidenced case, and the only one worth failing on: the baseline
// was built with a rule's findings in it, so its counts encode debt that rule
// found, and gating without that rule compares against a baseline the run
// cannot honour. A standalone repo whose baseline was generated on the same
// standalone backend skips the same rules in both runs and is not a regression.
//
// A baseline with no coverage record at all (written before mache-ddf14b, or by
// an older binary) yields nothing here. Absence is not evidence of full
// coverage, and failing on an unknown would break every such repo on upgrade;
// the skip is still REPORTED on the summary line, which is what closes the
// original hole. Regenerating the baseline records coverage and restores the
// stronger check.
func coverageRegression(now []skippedRule, base smellBaseline) []string {
	if base.RulesSkipped == nil {
		return nil // no record: report, do not fail
	}
	var lost []string
	for _, s := range now {
		if !slices.Contains(base.RulesSkipped, s.ID) {
			lost = append(lost, s.ID)
		}
	}
	slices.Sort(lost)
	return lost
}

// renderCoverage writes what the run did NOT assess, so a green gate cannot be
// read as a clean one without also reading why it was green.
//
// scanned is the number of rules that actually ran, so the line reports a
// fraction rather than a bare count — "2 rules skipped" means nothing without
// knowing whether 2 or 200 ran.
func renderCoverage(w io.Writer, scanned int, skipped []skippedRule, pathKeyed bool) {
	if len(skipped) > 0 {
		parts := make([]string, 0, len(skipped))
		for _, s := range skipped {
			parts = append(parts, fmt.Sprintf("%s (needs %s)", s.ID, strings.Join(s.Missing, ", ")))
		}
		slices.Sort(parts)
		_, _ = fmt.Fprintf(w, "  SKIPPED %d of %d rules — not assessed: %s\n",
			len(skipped), scanned+len(skipped), strings.Join(parts, "; "))
	}
	if pathKeyed {
		_, _ = fmt.Fprintf(w,
			"  DEGRADED this producer emits no _ast.node_hash, so the baseline is keyed by PATH: "+
				"moving a file reads as new debt (mache-dd45a3)\n")
	}
}

// renderCoverageRegression explains a coverage failure in terms of the decision
// it invalidates, not just the fact of the skip.
func renderCoverageRegression(w io.Writer, lost []string) {
	_, _ = fmt.Fprintf(w,
		"smell ratchet: COVERAGE REGRESSION — %d rule(s) contributed to this baseline but did not run now: %s\n",
		len(lost), strings.Join(lost, ", "))
	_, _ = fmt.Fprintln(w,
		"  the baseline's counts include findings from those rules, so \"no new debt\" is not a claim this run can make.")
	_, _ = fmt.Fprintln(w,
		"  build the .db with a producer that supplies the missing tables (leyline parse), or regenerate the baseline on this backend.")
}
