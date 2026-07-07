package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

// dbQuerier wraps a *sql.DB so it satisfies refsQuerier — the only
// interface runSmellRule / missingTables / populateSnippets need.
// Lets the CLI run rules against any SQLite db (mache-built or
// LLO-built) without touching the graph stack.
type dbQuerier struct {
	db   *sql.DB
	path string // mache-190508 step 3: source for sibling .bindings.capnp lookup
}

func (q *dbQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

// DBPath implements the dbPathProvider opt-in (cmd/serve_find_smells.go)
// so canonical-view setup can find the sibling .bindings.capnp event
// log next to this .db.
func (q *dbQuerier) DBPath() string { return q.path }

var (
	findSmellsDBPath    string
	findSmellsRule      string
	findSmellsSourceID  string
	findSmellsLimit     int
	findSmellsMinMetric int64
	findSmellsFormat    string
	findSmellsFailOn    string
	findSmellsTags      string
	findSmellsRulesDir  string

	// findSmellsBaseline gates on new-findings-vs-baseline (the W5 ratchet):
	// when set, exit non-zero iff any (rule,file) exceeds the committed
	// baseline count, ignoring --fail-on. findSmellsWriteBaseline regenerates
	// the committed baseline from the current scan. Bead mache-4d155c.
	findSmellsBaseline      string
	findSmellsWriteBaseline string
	// findSmellsBaselineRoot relativizes source_id (which mache build records
	// absolute) against this prefix before baselining/gating, so a committed
	// baseline is portable across machines and CI. Bead mache-4da90e.
	findSmellsBaselineRoot string
)

// exitFunc is the process-exit hook the CLI uses after RunE returns a
// non-zero code. Production points it at os.Exit; tests override it to
// capture the code without crashing the test binary. Keeping it as a
// package var (rather than weaving t.Cleanup hooks through every test)
// matches the cobra-CLI convention used elsewhere in this package.
var exitFunc = os.Exit

var findSmellsCmd = &cobra.Command{
	Use:   "find-smells",
	Short: "Run structural code-smell rules against a mache or leyline-built SQLite database",
	Long: `Runs the same rule registry that powers the find_smells MCP tool, but as a
CLI for non-MCP consumers (CI, scripts, GitHub Actions). With no --rule
argument it lists registered rules; otherwise it executes the named rule
(or every rule whose ID matches the glob pattern).

Output formats:
  --format=json (default)  machine-readable, same shape as the MCP tool
  --format=md              human-readable markdown summary
  --format=ci              one-line-per-finding "file:line:col: severity: msg [rule-id]"
                           (gh / ripgrep / vale convention)
  --format=sarif           SARIF 2.1.0 (one document over all rules; for GitHub code-scanning)

Rule selection:
  --rule=ID                exact match (legacy)
  --rule='drift_doc_*'     filepath.Match glob; runs every matching rule
  --tags=docs,security     filter the registry to rules whose Tags contain
                           ANY of the listed values (set union). Composes
                           with --rule glob.

Gate decision (ADR-0018, pylint precedent):
  --fail-on=none           never exits non-zero on findings (legacy default)
  --fail-on=warn           exit 1 if any finding's rule resolves to warn or error
  --fail-on=error (default) exit 1 only on findings from rules with Severity=error

Exit codes:
  0  success — no findings exceeded the --fail-on threshold
  1  findings exceeded --fail-on threshold (new in ADR-0018 PR 2)
  2  pre-flight failed (missing tables on the active backend)
  3  unknown rule (no matches from --rule glob + --tags filter)
  4  database open / query error

The historical observability contract ("never exits non-zero on findings") is
preserved with --fail-on=none, and is the de-facto default for every rule that
ships at Severity=warn (which is every rule today) when --fail-on=error.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		code, err := runFindSmells(cmd, args)
		// On success (code 0), return any benign render error directly so
		// cobra surfaces it via its normal channel. On non-zero, the
		// error has already been printed to stderr inside runFindSmells;
		// short-circuit via exitFunc so the process exit code reflects
		// the failure mode (2 = pre-flight, 3 = unknown rule, 4 = db,
		// 1 = gate fired). Tests swap exitFunc to capture the code.
		if code != 0 {
			exitFunc(code)
		}
		return err
	},
}

// runFindSmells is the testable core: it returns the intended process
// exit code plus any cobra-surfaceable error, instead of calling
// os.Exit directly. This lets tests assert exit semantics (especially
// the new --fail-on gate behavior) without crashing the test binary.
//
// Production flow:
//
//	RunE -> runFindSmells -> (code, err) -> exitFunc(code) on non-zero
//
// Error printing for non-zero codes still happens here (mirroring the
// old cliExit), so cobra doesn't double-print.
func runFindSmells(cmd *cobra.Command, _ []string) (int, error) {
	// Resolve the active rule set (built-ins + external) fresh on every
	// invocation so a rule JSON added to the dir since the last run is
	// picked up with no rebuild. projectDir is the process working
	// directory: the CLI is run from the repo root in CI (where
	// .mache.json lives), matching how `mache serve` treats its base
	// path. Precedence: --rules-dir > MACHE_SMELL_RULES_DIR > .mache.json.
	cwd, _ := os.Getwd()
	rulesDir := resolveSmellRulesDir(findSmellsRulesDir, cwd)
	activeRules, err := activeSmellRules(rulesDir)
	if err != nil {
		// The CLI surfaces external-load errors loudly (a bad rule file
		// should fail the run, not be silently dropped). Exit 4 = the
		// general db/config error class.
		return printAndCode(4, fmt.Errorf("load external smell rules from %s: %w", rulesDir, err))
	}

	// Discovery mode: no rule glob -> dump the listing. No db required.
	// Tags filter is intentionally NOT applied to discovery — listing
	// is meant to show every registered rule so an agent can decide
	// which tags to pass.
	if findSmellsRule == "" {
		return 0, renderListing(cmd.OutOrStdout(), activeRules, findSmellsFormat)
	}

	// Validate --fail-on early so a typo surfaces before we open the db.
	if !validFailOn(findSmellsFailOn) {
		return printAndCode(4, fmt.Errorf(
			"invalid --fail-on=%q; must be one of: none, warn, error", findSmellsFailOn,
		))
	}

	// Resolve rule selection: glob + tags filter against the registry.
	// allRuleIDs() returns alphabetical; matchRules preserves registry
	// order so multiple-match runs are deterministic.
	tagSet := parseTags(findSmellsTags)
	matched, err := matchRules(activeRules, findSmellsRule, tagSet)
	if err != nil {
		return printAndCode(3, fmt.Errorf(
			"invalid --rule glob %q: %w", findSmellsRule, err,
		))
	}
	if len(matched) == 0 {
		return printAndCode(3, fmt.Errorf(
			"no rules match --rule=%q --tags=%q — available: %s — run with no --rule for full descriptions",
			findSmellsRule, findSmellsTags, strings.Join(allRuleIDs(activeRules), ", "),
		))
	}

	// Running rules requires a db. SQLite would silently create
	// missing files on Open, so existence-check first to give a
	// clear error rather than running against an empty auto-created file.
	if findSmellsDBPath == "" {
		return 0, fmt.Errorf("--db PATH is required when --rule is set")
	}
	if _, err := os.Stat(findSmellsDBPath); err != nil {
		return printAndCode(4, fmt.Errorf("db file: %w", err))
	}
	db, err := sql.Open("sqlite", findSmellsDBPath)
	if err != nil {
		return printAndCode(4, fmt.Errorf("open db: %w", err))
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return printAndCode(4, fmt.Errorf("ping db: %w", err))
	}
	// SetMaxOpenConns(1) pins all queries to a single connection so
	// the TEMP _capnp_binding_refs table created by ensureCanonicalViews
	// + LoadCapnpBindings stays visible to subsequent queries.
	// modernc/sqlite spreads queries across the pool by default,
	// which would put the TEMP-table population on a connection
	// the rule SELECT never sees.
	db.SetMaxOpenConns(1)
	qg := &dbQuerier{db: db, path: findSmellsDBPath}

	// Mirror the MCP handler's threshold-default semantics. If
	// the caller passed --min-metric explicitly, that value wins
	// (including 0 for "show me everything sorted"). If the flag
	// wasn't set, fall back to rule.DefaultMinMetric per rule.
	minMetricChanged := cmd.Flags().Changed("min-metric")

	// Run every matched rule and concatenate results. Each finding
	// already carries its RuleID so downstream consumers can split
	// by rule. Output is one envelope per rule in JSON/MD modes; CI
	// mode interleaves lines (per the gh/ripgrep convention).
	results := make([]ruleRunResult, 0, len(matched))

	for _, rule := range matched {
		// Pre-flight required tables, same shape as the MCP handler.
		if missing, mErr := missingTables(qg, rule.Requires); mErr != nil {
			return printAndCode(4, fmt.Errorf("pre-flight: %w", mErr))
		} else if len(missing) > 0 {
			// A glob/tag matched multiple rules: skip the ones whose tables
			// aren't present (e.g. _ast rules on a pure-Go tree-sitter .db)
			// rather than aborting the whole run — the gate assesses what it
			// CAN. An explicitly-named single rule still errors (you asked for
			// exactly it). Bead mache-4da90e (W5.3).
			if len(matched) > 1 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"find-smells: skipping rule %q — requires absent tables [%s]\n",
					rule.ID, strings.Join(missing, ", "))
				continue
			}
			backendNote := ""
			if backend := queryBuildBackend(qg); backend != "" {
				backendNote = fmt.Sprintf(" (built with backend=%q)", backend)
			}
			return printAndCode(2, fmt.Errorf(
				"rule %q requires SQL tables [%s] which aren't present in %s%s — "+
					"_ast / _source / _imports / _lsp* come from ley-line-open's `leyline parse`; "+
					"node_defs / node_refs / nodes come from both standalone mache and LLO; "+
					"see docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full capability matrix",
				rule.ID, strings.Join(missing, ", "), findSmellsDBPath, backendNote,
			))
		}

		findings, rErr := runSmellRule(qg, rule, findSmellsSourceID, findSmellsLimit)
		if rErr != nil {
			return printAndCode(4, fmt.Errorf("rule %q: %w", rule.ID, rErr))
		}

		minMetric := findSmellsMinMetric
		if !minMetricChanged {
			minMetric = rule.DefaultMinMetric
		}
		if minMetric > 0 {
			filtered := findings[:0]
			for _, f := range findings {
				if f.Metric >= minMetric {
					filtered = append(filtered, f)
				}
			}
			findings = filtered
		}
		populateSnippets(qg, findings)
		results = append(results, ruleRunResult{rule: rule, findings: findings})
	}

	// Render — CI + SARIF render once across all rules; JSON/MD emit one
	// envelope per rule so the legacy single-rule shape is preserved.
	switch findSmellsFormat {
	case "ci":
		for _, r := range results {
			if err := renderFindingsCI(cmd.OutOrStdout(), r.rule, r.findings); err != nil {
				return 0, err
			}
		}
	case "sarif":
		if err := renderSARIF(cmd.OutOrStdout(), results, findSmellsBaselineRoot); err != nil {
			return 0, err
		}
	default:
		for _, r := range results {
			if err := renderFindings(cmd.OutOrStdout(), r.rule.ID, r.findings, findSmellsFormat); err != nil {
				return 0, err
			}
		}
	}

	// W5 ratchet (mache-4d155c): --write-baseline regenerates the committed
	// baseline (grandfathers current findings); --baseline gates on
	// new-findings-vs-baseline, overriding --fail-on. Both operate on the
	// flattened finding set.
	scanned := relativizeFindings(allFindings(results), findSmellsBaselineRoot)
	if findSmellsWriteBaseline != "" {
		if err := writeBaseline(findSmellsWriteBaseline, computeBaseline(scanned)); err != nil {
			return printAndCode(4, fmt.Errorf("write baseline %s: %w", findSmellsWriteBaseline, err))
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote smell baseline: %s\n", findSmellsWriteBaseline)
		return 0, nil
	}
	if findSmellsBaseline != "" {
		base, err := loadBaseline(findSmellsBaseline)
		if err != nil {
			return printAndCode(4, fmt.Errorf("load baseline %s: %w", findSmellsBaseline, err))
		}
		if debt := newDebt(scanned, base); len(debt) > 0 {
			renderNewDebt(cmd.ErrOrStderr(), debt)
			return 1, nil
		}
		return 0, nil
	}

	// Gate decision (ADR-0018). --fail-on=none preserves the legacy
	// observability contract; warn fails on warn-or-error findings;
	// error fails only on findings from rules opted-in via
	// Severity=error. Today every shipped rule is warn, so the
	// effective default behavior with --fail-on=error is "exit 0".
	exitCode := gateDecision(findSmellsFailOn, results)
	return exitCode, nil
}

// printAndCode mirrors the historical cliExit side effect (write the
// error to stderr) but returns the intended exit code instead of
// calling os.Exit, so tests can assert it.
func printAndCode(code int, err error) (int, error) {
	fmt.Fprintln(os.Stderr, err)
	return code, nil
}

// validFailOn returns true for the three permitted --fail-on values.
// Anything else is a typo; surfacing it as exit 4 (DB-error class)
// keeps the unknown-rule code (3) reserved for rule-resolution failures.
func validFailOn(s string) bool {
	switch s {
	case "none", "warn", "error":
		return true
	}
	return false
}

// parseTags splits the comma-separated --tags value into a lookup set.
// Empty / whitespace tokens are dropped so `--tags=,foo,` behaves like
// `--tags=foo`.
func parseTags(csv string) map[string]struct{} {
	if csv == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, t := range strings.Split(csv, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out[t] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchRules resolves a --rule pattern (exact ID or glob) intersected
// with the --tags filter to the concrete subset of the given rule set
// that should run. The pattern is matched via filepath.Match, which
// supports `*`, `?`, and `[...]`. A pattern without metacharacters
// behaves as exact-match (same as the legacy code path).
//
// The rules argument is the active set (built-ins + external) the caller
// resolved for this invocation, so external rules are selectable too.
// Pointers into that slice are returned, so it must outlive the result.
//
// Tags semantics are union (match-any): a rule passes when ANY of its
// Tags is in the requested set. Empty tagSet disables the filter.
//
// Registry order is preserved so multi-match output is deterministic
// across runs.
func matchRules(rules []SmellRule, pattern string, tagSet map[string]struct{}) ([]*SmellRule, error) {
	// Eagerly validate the pattern so a typo like '[' surfaces here,
	// not deep inside the loop where filepath.Match would return
	// ErrBadPattern repeatedly.
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	var out []*SmellRule
	for i := range rules {
		r := &rules[i]
		ok, _ := filepath.Match(pattern, r.ID)
		if !ok {
			continue
		}
		if len(tagSet) > 0 && !hasAnyTag(r.Tags, tagSet) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// hasAnyTag implements the union-semantics tag filter: returns true
// if the rule carries at least one of the requested tags.
func hasAnyTag(ruleTags []string, want map[string]struct{}) bool {
	for _, t := range ruleTags {
		if _, ok := want[t]; ok {
			return true
		}
	}
	return false
}

// ruleRunResult bundles one rule with the findings it produced for the
// current invocation. Package-level (not anonymous) so gateDecision can
// take a typed slice — anonymous structs don't share identity across
// function boundaries.
type ruleRunResult struct {
	rule     *SmellRule
	findings []smellFinding
}

// gateDecision implements --fail-on per ADR-0018. Returns 1 when any
// finding belongs to a rule whose Effective() severity crosses the
// requested threshold; 0 otherwise. --fail-on=none always returns 0
// (the legacy observability contract).
func gateDecision(failOn string, results []ruleRunResult) int {
	if failOn == "none" {
		return 0
	}
	for _, r := range results {
		if len(r.findings) == 0 {
			continue
		}
		sev := r.rule.Effective()
		switch failOn {
		case "warn":
			if sev == SeverityWarn || sev == SeverityError {
				return 1
			}
		case "error":
			if sev == SeverityError {
				return 1
			}
		}
	}
	return 0
}

func renderListing(w io.Writer, rules []SmellRule, format string) error {
	listing := rulesListing(rules)
	if format == "md" {
		return renderListingMD(w, listing)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(listing)
}

func renderListingMD(w io.Writer, listing any) error {
	// rulesListing returns an anonymous struct; round-trip through JSON
	// so we don't reach into its private shape from this package.
	raw, err := json.Marshal(listing)
	if err != nil {
		return err
	}
	var parsed struct {
		Help  string `json:"help"`
		Rules []struct {
			ID          string   `json:"id"`
			Languages   []string `json:"languages,omitempty"`
			Description string   `json:"description"`
			Requires    []string `json:"requires,omitempty"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("# find_smells rules\n\n")
	sb.WriteString(parsed.Help)
	sb.WriteString("\n\n")
	for _, r := range parsed.Rules {
		fmt.Fprintf(&sb, "## `%s`\n\n", r.ID)
		if len(r.Languages) > 0 {
			fmt.Fprintf(&sb, "**Languages:** %s\n\n", strings.Join(r.Languages, ", "))
		}
		if len(r.Requires) > 0 {
			fmt.Fprintf(&sb, "**Requires tables:** %s\n\n", strings.Join(r.Requires, ", "))
		}
		sb.WriteString(r.Description)
		sb.WriteString("\n\n")
	}
	_, err = io.WriteString(w, sb.String())
	return err
}

func renderFindings(w io.Writer, ruleID string, findings []smellFinding, format string) error {
	// Emit an empty finding set as `[]`, not `null`: a nil slice marshals to
	// null, which forces json consumers to special-case it and diverges from
	// --format sarif (which always emits arrays). Zero findings is the common
	// clean-gate case, so keep the shape a stable array.
	if findings == nil {
		findings = []smellFinding{}
	}
	resp := struct {
		Rule     string         `json:"rule"`
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}{Rule: ruleID, Total: len(findings), Findings: findings}

	if format == "md" {
		var sb strings.Builder
		fmt.Fprintf(&sb, "# find_smells: `%s`\n\n", ruleID)
		fmt.Fprintf(&sb, "**%d findings**\n\n", len(findings))
		if len(findings) == 0 {
			sb.WriteString("No findings on this run.\n")
		} else {
			sb.WriteString("| Source | Node | Line | Metric |\n")
			sb.WriteString("| --- | --- | ---: | ---: |\n")
			for _, f := range findings {
				fmt.Fprintf(&sb, "| %s | %s | %d | %d |\n",
					escapePipes(f.SourceID), escapePipes(f.NodeID), f.Line, f.Metric)
			}
		}
		_, err := io.WriteString(w, sb.String())
		return err
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(resp)
}

// renderFindingsCI emits one line per finding in the gh/ripgrep/vale
// convention: `<source_id>:<line>:<column>: <severity>: <message> [<rule-id>]`.
// The message is the rule's Description, normalized (newlines -> spaces,
// truncated to ~120 chars) so editors can parse the line cleanly.
// Severity is the rule's Effective() value so the on-the-wire shape
// reflects the gate decision the consumer would see.
func renderFindingsCI(w io.Writer, rule *SmellRule, findings []smellFinding) error {
	sev := string(rule.Effective())
	msg := truncateOneLine(rule.Description, 120)
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "%s:%d:%d: %s: %s [%s]\n",
			f.SourceID, f.Line, f.Column, sev, msg, rule.ID,
		); err != nil {
			return err
		}
	}
	return nil
}

// truncateOneLine collapses newlines/tabs/CRs to single spaces and
// truncates at maxLen with an ellipsis, so a multiline rule description
// renders cleanly on the CI format's one-line shape. The cap is fuzzy
// (gh/vale don't enforce a hard limit) but ~120 keeps lines reviewable
// in a terminal without horizontal scroll on most setups.
func truncateOneLine(s string, maxLen int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	// Collapse runs of spaces so the output stays compact.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		// Reserve 3 chars for the ellipsis so the total length stays
		// within maxLen for downstream parsers that buffer line-by-line.
		if maxLen > 3 {
			s = s[:maxLen-3] + "..."
		} else {
			s = s[:maxLen]
		}
	}
	return s
}

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func init() {
	findSmellsCmd.Flags().StringVar(&findSmellsDBPath, "db", "", "path to the SQLite database (mache-built or leyline-built) — required")
	findSmellsCmd.Flags().StringVar(&findSmellsRule, "rule", "", "rule ID or glob (e.g. 'drift_doc_*'); omit to list available rules")
	findSmellsCmd.Flags().StringVar(&findSmellsSourceID, "source-id", "", "scope rule to a single source file path")
	findSmellsCmd.Flags().IntVar(&findSmellsLimit, "limit", 200, "cap result count per rule")
	findSmellsCmd.Flags().Int64Var(&findSmellsMinMetric, "min-metric", 0, "drop findings whose metric is below this threshold")
	findSmellsCmd.Flags().StringVar(&findSmellsFormat, "format", "json", "output format: json, md, ci, or sarif")
	findSmellsCmd.Flags().StringVar(&findSmellsFailOn, "fail-on", "error", "exit non-zero when findings reach severity: none | warn | error (ADR-0018)")
	findSmellsCmd.Flags().StringVar(&findSmellsTags, "tags", "", "comma-separated tags; runs rules whose Tags contain ANY of these values (set union)")
	findSmellsCmd.Flags().StringVar(&findSmellsRulesDir, "rules-dir", "", "directory of external SmellRule JSON files to merge with the built-ins (overrides MACHE_SMELL_RULES_DIR and .mache.json smellRulesDir); rescanned every run")
	findSmellsCmd.Flags().StringVar(&findSmellsBaseline, "baseline", "", "path to a committed smell baseline JSON; exit non-zero only on findings that EXCEED the baseline count per (rule,file) — the W5 ratchet")
	findSmellsCmd.Flags().StringVar(&findSmellsWriteBaseline, "write-baseline", "", "regenerate the baseline JSON at this path from the current scan (grandfathers all current findings), then exit 0")
	findSmellsCmd.Flags().StringVar(&findSmellsBaselineRoot, "baseline-root", "", "trim this path prefix from source_id before baselining/gating so a committed baseline is machine/CI-portable (e.g. --baseline-root \"$PWD\")")
	rootCmd.AddCommand(findSmellsCmd)
}
