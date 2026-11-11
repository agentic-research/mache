package smells

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/agentic-research/mache/graph"
)

// The find-smells run, one phase per function: which rules, over which db,
// with what results, rendered how. runFindSmells (find_smells_cli.go) strings
// them together; the exit codes it documents are decided here.

// selectRules resolves the rules this invocation runs: the active set
// (built-ins + external), the listing when no --rule is given, --fail-on
// validated early, then the --rule glob and --tags filter. A nil result means
// the run is over — the listing was rendered or the error printed — and code
// and err are what runFindSmells returns.
func selectRules(cmd *cobra.Command) ([]*SmellRule, int, error) {
	// Resolve the active rule set fresh on every invocation so a rule JSON
	// added to the dir since the last run is picked up with no rebuild.
	// projectDir is the process working directory: the CLI is run from the
	// repo root in CI (where .mache.json lives), matching how `mache serve`
	// treats its base path. Precedence: --rules-dir > MACHE_SMELL_RULES_DIR
	// > .mache.json.
	cwd, _ := os.Getwd()
	rulesDir := ResolveSmellRulesDir(findSmellsRulesDir, cwd)
	activeRules, err := activeSmellRules(rulesDir)
	if err != nil {
		// The CLI surfaces external-load errors loudly (a bad rule file
		// should fail the run, not be silently dropped). Exit 4 = the
		// general db/config error class.
		code, err := printAndCode(4, fmt.Errorf("load external smell rules from %s: %w", rulesDir, err))
		return nil, code, err
	}

	// Discovery mode: no rule glob -> dump the listing. No db required.
	// Tags filter is intentionally NOT applied to discovery — listing
	// is meant to show every registered rule so an agent can decide
	// which tags to pass.
	if findSmellsRule == "" {
		return nil, 0, renderListing(cmd.OutOrStdout(), activeRules, findSmellsFormat)
	}

	// Validate --fail-on early so a typo surfaces before we open the db.
	if !validFailOn(findSmellsFailOn) {
		code, err := printAndCode(4, fmt.Errorf(
			"invalid --fail-on=%q; must be one of: none, warn, error", findSmellsFailOn,
		))
		return nil, code, err
	}

	// Resolve rule selection: glob + tags filter against the registry.
	// allRuleIDs() returns alphabetical; matchRules preserves registry
	// order so multiple-match runs are deterministic.
	matched, err := matchRules(activeRules, findSmellsRule, parseTags(findSmellsTags))
	if err != nil {
		code, err := printAndCode(3, fmt.Errorf("invalid --rule glob %q: %w", findSmellsRule, err))
		return nil, code, err
	}
	if len(matched) == 0 {
		code, err := printAndCode(3, fmt.Errorf(
			"no rules match --rule=%q --tags=%q — available: %s — run with no --rule for full descriptions",
			findSmellsRule, findSmellsTags, strings.Join(allRuleIDs(activeRules), ", "),
		))
		return nil, code, err
	}
	return matched, 0, nil
}

// printAndCode mirrors the historical cliExit side effect (write the
// error to stderr) but returns the intended exit code instead of
// calling os.Exit, so tests can assert it.
// openSmellDB opens the db the rules run against, or says why it cannot be
// run against (every error is the exit-4 db class). SQLite would silently
// create a missing file on Open, so existence is checked first to give a
// clear error rather than a run against an empty auto-created file.
func openSmellDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("db file: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	// SetMaxOpenConns(1) pins all queries to a single connection so
	// the TEMP _capnp_binding_refs table created by EnsureCanonicalViews
	// + LoadCapnpBindings stays visible to subsequent queries.
	// modernc/sqlite spreads queries across the pool by default,
	// which would put the TEMP-table population on a connection
	// the rule SELECT never sees.
	db.SetMaxOpenConns(1)
	// The rules read _ast, node_defs and node_refs by column, so a db of a
	// shape mache has not been checked against is refused here as it is on
	// every graph open, rather than scored against the wrong meaning.
	if err := graph.CheckProjection(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	return db, nil
}

// runMatchedRules runs every matched rule and concatenates the results. Each
// finding already carries its RuleID so downstream consumers can split by
// rule. Skips are collected, not just warned about: the ratchet and SARIF
// both need to say what this run did NOT assess (mache-ddf14b). A nil result
// means the run is over with the error printed, and code and err are what
// runFindSmells returns.
func runMatchedRules(cmd *cobra.Command, qg *dbQuerier, matched []*SmellRule) ([]ruleRunResult, []skippedRule, int, error) {
	// Mirror the MCP handler's threshold-default semantics. If
	// the caller passed --min-metric explicitly, that value wins
	// (including 0 for "show me everything sorted"). If the flag
	// wasn't set, fall back to rule.DefaultMinMetric per rule.
	minMetricChanged := cmd.Flags().Changed("min-metric")

	results := make([]ruleRunResult, 0, len(matched))
	var skipped []skippedRule
	for _, rule := range matched {
		missing, code, err := preflightRule(cmd, qg, rule, len(matched) > 1)
		if code != 0 {
			return nil, nil, code, err
		}
		if len(missing) > 0 {
			skipped = append(skipped, skippedRule{ID: rule.ID, Missing: missing})
			continue
		}

		findings, rErr := runSmellRuleQuery(qg, rule, findSmellsSourceID, findSmellsLimit)
		if rErr != nil {
			code, err := printAndCode(4, fmt.Errorf("rule %q: %w", rule.ID, rErr))
			return nil, nil, code, err
		}
		minMetric := findSmellsMinMetric
		if !minMetricChanged {
			minMetric = rule.DefaultMinMetric
		}
		findings = aboveMinMetric(findings, minMetric)
		populateSnippets(qg, findings)
		results = append(results, ruleRunResult{rule: rule, findings: findings})
	}
	return results, skipped, 0, nil
}

// preflightRule checks the SQL tables rule requires, same shape as the MCP
// handler. When a glob/tag matched several rules, one whose tables are
// absent (e.g. _ast rules on a pure-Go tree-sitter .db) is skipped rather
// than aborting the whole run — the gate assesses what it CAN — and its
// missing tables are returned for the skip record. An explicitly-named
// single rule still errors (you asked for exactly it), with exit 2. Bead
// mache-4da90e (W5.3). A nonzero code ends the run.
func preflightRule(cmd *cobra.Command, qg *dbQuerier, rule *SmellRule, multi bool) ([]string, int, error) {
	missing, mErr := missingTables(qg, rule.Requires)
	if mErr != nil {
		code, err := printAndCode(4, fmt.Errorf("pre-flight: %w", mErr))
		return nil, code, err
	}
	if len(missing) == 0 {
		return nil, 0, nil
	}
	if multi {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"find-smells: skipping rule %q — requires absent tables [%s]\n",
			rule.ID, strings.Join(missing, ", "))
		return missing, 0, nil
	}
	backendNote := ""
	if backend := queryBuildBackend(qg); backend != "" {
		backendNote = fmt.Sprintf(" (built with backend=%q)", backend)
	}
	code, err := printAndCode(2, fmt.Errorf(
		"rule %q requires SQL tables [%s] which aren't present in %s%s — "+
			"_ast / _source / _imports / _lsp* come from ley-line-open's `leyline parse`; "+
			"node_defs / node_refs / nodes come from both standalone mache and LLO; "+
			"see docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full capability matrix",
		rule.ID, strings.Join(missing, ", "), findSmellsDBPath, backendNote,
	))
	return nil, code, err
}

// aboveMinMetric keeps the findings at or above minMetric, in place; a
// threshold of 0 keeps everything ("show me everything sorted").
func aboveMinMetric(findings []smellFinding, minMetric int64) []smellFinding {
	if minMetric <= 0 {
		return findings
	}
	filtered := findings[:0]
	for _, f := range findings {
		if f.Metric >= minMetric {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// renderResults writes the run in --format. CI and SARIF render once across
// all rules; JSON and MD emit one envelope per rule so the legacy
// single-rule shape is preserved.
func renderResults(w io.Writer, results []ruleRunResult, skipped []skippedRule) error {
	switch findSmellsFormat {
	case "ci":
		for _, r := range results {
			if err := renderFindingsCI(w, r.rule, r.findings); err != nil {
				return err
			}
		}
		return nil
	case "sarif":
		return renderSARIF(w, results, findSmellsBaselineRoot, skipped)
	default:
		for _, r := range results {
			if err := renderFindings(w, r.rule.ID, r.findings, findSmellsFormat, findSmellsLimit); err != nil {
				return err
			}
		}
		return nil
	}
}
