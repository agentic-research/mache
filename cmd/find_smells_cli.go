package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

// dbQuerier wraps a *sql.DB so it satisfies refsQuerier — the only
// interface runSmellRule / missingTables / populateSnippets need.
// Lets the CLI run rules against any SQLite db (mache-built or
// LLO-built) without touching the graph stack.
type dbQuerier struct{ db *sql.DB }

func (q *dbQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

var (
	findSmellsDBPath    string
	findSmellsRule      string
	findSmellsSourceID  string
	findSmellsLimit     int
	findSmellsMinMetric int64
	findSmellsFormat    string
)

var findSmellsCmd = &cobra.Command{
	Use:   "find-smells",
	Short: "Run structural code-smell rules against a mache or leyline-built SQLite database",
	Long: `Runs the same rule registry that powers the find_smells MCP tool, but as a
CLI for non-MCP consumers (CI, scripts, GitHub Actions). With no --rule
argument it lists registered rules; otherwise it executes the named rule.

Output formats:
  --format=json (default)  machine-readable, same shape as the MCP tool
  --format=md              human-readable markdown summary

Exit codes:
  0  success — regardless of how many findings the rule produced
  2  pre-flight failed (missing tables on the active backend)
  3  unknown rule
  4  database open / query error

This tool is observability, not a gate. It never exits non-zero on findings.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Discovery mode: no rule -> dump the listing. No db required.
		if findSmellsRule == "" {
			return renderListing(cmd.OutOrStdout(), findSmellsFormat)
		}

		// Running a rule requires a db. SQLite would silently create
		// missing files on Open, so existence-check first to give a
		// clear error rather than running the rule against an empty
		// auto-created file.
		if findSmellsDBPath == "" {
			return fmt.Errorf("--db PATH is required when --rule is set")
		}
		if _, err := os.Stat(findSmellsDBPath); err != nil {
			return cliExit(4, fmt.Errorf("db file: %w", err))
		}
		db, err := sql.Open("sqlite", findSmellsDBPath)
		if err != nil {
			return cliExit(4, fmt.Errorf("open db: %w", err))
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			return cliExit(4, fmt.Errorf("ping db: %w", err))
		}
		qg := &dbQuerier{db: db}

		var rule *SmellRule
		for i := range smellRegistry {
			if smellRegistry[i].ID == findSmellsRule {
				rule = &smellRegistry[i]
				break
			}
		}
		if rule == nil {
			return cliExit(3, fmt.Errorf(
				"unknown rule %q — available: %s — run with no --rule for full descriptions",
				findSmellsRule, strings.Join(allRuleIDs(), ", "),
			))
		}

		// Pre-flight required tables, same shape as the MCP handler.
		if missing, err := missingTables(qg, rule.Requires); err != nil {
			return cliExit(4, fmt.Errorf("pre-flight: %w", err))
		} else if len(missing) > 0 {
			return cliExit(2, fmt.Errorf(
				"rule %q requires SQL tables [%s] which aren't present in %s — "+
					"_ast / _source / _imports / _lsp* come from ley-line-open's `leyline parse`; "+
					"node_defs / node_refs / nodes come from both standalone mache and LLO; "+
					"see docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full capability matrix",
				findSmellsRule, strings.Join(missing, ", "), findSmellsDBPath,
			))
		}

		findings, err := runSmellRule(qg, rule, findSmellsSourceID, findSmellsLimit)
		if err != nil {
			return cliExit(4, fmt.Errorf("rule %q: %w", findSmellsRule, err))
		}

		if findSmellsMinMetric > 0 {
			filtered := findings[:0]
			for _, f := range findings {
				if f.Metric >= findSmellsMinMetric {
					filtered = append(filtered, f)
				}
			}
			findings = filtered
		}
		populateSnippets(qg, findings)

		return renderFindings(cmd.OutOrStdout(), rule.ID, findings, findSmellsFormat)
	},
}

// cliExit wraps an error with a specific exit code so callers can
// distinguish pre-flight failures from real errors. Cobra's RunE only
// supports a single err return, so we set the process exit code via
// os.Exit at the call site.
func cliExit(code int, err error) error {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(code)
	return err // unreachable
}

func renderListing(w io.Writer, format string) error {
	listing := rulesListing()
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

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func init() {
	findSmellsCmd.Flags().StringVar(&findSmellsDBPath, "db", "", "path to the SQLite database (mache-built or leyline-built) — required")
	findSmellsCmd.Flags().StringVar(&findSmellsRule, "rule", "", "rule ID to run; omit to list available rules")
	findSmellsCmd.Flags().StringVar(&findSmellsSourceID, "source-id", "", "scope rule to a single source file path")
	findSmellsCmd.Flags().IntVar(&findSmellsLimit, "limit", 200, "cap result count")
	findSmellsCmd.Flags().Int64Var(&findSmellsMinMetric, "min-metric", 0, "drop findings whose metric is below this threshold")
	findSmellsCmd.Flags().StringVar(&findSmellsFormat, "format", "json", "output format: json or md")
	rootCmd.AddCommand(findSmellsCmd)
}
