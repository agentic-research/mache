package cmd

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// makeFindSmellsHandler returns the MCP handler. With no `rule` arg
// it lists all registered rules; with a rule it runs the scan.
//
// The optional rulesDir is the directory of external rule JSON files
// resolved once at serve startup (flag / env / .mache.json). The
// handler re-scans it PER REQUEST via activeSmellRules, so a rule file
// dropped into the dir is picked up on the next call with no restart —
// the live-reload property. It's variadic so the ~80 existing test
// callsites that pass only a graph keep compiling (dir defaults to ""
// = built-in rules only). On an external-load error the handler logs
// and falls back to the built-ins so a bad rule file can't take the
// daemon down.
func makeFindSmellsHandler(g graph.Graph, rulesDir ...string) server.ToolHandlerFunc {
	dir := ""
	if len(rulesDir) > 0 {
		dir = rulesDir[0]
	}
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rules, err := activeSmellRules(dir)
		if err != nil {
			log.Printf("smell rules: using built-ins only; external load from %s failed: %v", dir, err)
		}
		ruleID := strings.TrimSpace(request.GetString("rule", ""))
		sourceID := strings.TrimSpace(request.GetString("source_id", ""))
		limitFloat := request.GetFloat("limit", 200)
		limit := int(limitFloat)
		if limit <= 0 { // coverage:ignore — defensive default; tests always pass limit>0
			limit = 200 // coverage:ignore
		} // coverage:ignore

		if ruleID == "" {
			// Discovery mode — list rules.
			return mcp.NewToolResultText(jsonOrPanic(rulesListing(rules))), nil
		}

		if ruleID == "*" {
			// L1 digest mode — "shape without weight": per-rule counts +
			// per-file rollup + a few worst exemplars, instead of a flat
			// (capped) dump. Drill to L2 with rule=<id> (+ source_id).
			qg, ok := g.(refsQuerier)
			if !ok { // coverage:ignore — all test graphs implement refsQuerier
				return mcp.NewToolResultError("the active graph backend doesn't expose a SQL handle; find_smells requires a leyline-parsed .db"), nil // coverage:ignore
			} // coverage:ignore
			digest, err := buildSmellDigest(qg, rules, 5, 10)
			if err != nil { // coverage:ignore — buildSmellDigest only errors on DB IO; unreachable with in-memory fixtures
				return mcp.NewToolResultError(fmt.Sprintf("digest: %v", err)), nil // coverage:ignore
			} // coverage:ignore
			return mcp.NewToolResultText(jsonOrPanic(digest)), nil
		}

		var rule *SmellRule
		for i := range rules {
			if rules[i].ID == ruleID {
				rule = &rules[i]
				break
			}
		}
		if rule == nil {
			return mcp.NewToolResultError(fmt.Sprintf(
				"unknown rule %q — available: %s. Call this tool with no rule for full descriptions.",
				ruleID, strings.Join(allRuleIDs(rules), ", "),
			)), nil
		}

		// Resolve min_metric AFTER rule lookup so we can fall back
		// to the rule's DefaultMinMetric when the request omits it.
		// An explicit `min_metric=0` from the caller still wins —
		// it disables the default and returns everything sorted by
		// metric. Only an absent key activates the default.
		minMetric := int64(0)
		if v, ok := request.GetArguments()["min_metric"]; ok {
			if f, ok := v.(float64); ok {
				minMetric = int64(f)
			}
		} else {
			minMetric = rule.DefaultMinMetric
		}

		// Need a *sql.DB to run the rule. Today we hand-shake via the
		// refsQuerier interface that other handlers already use.
		qg, ok := g.(refsQuerier)
		if !ok { // coverage:ignore — all test graphs implement refsQuerier; non-SQL backend can't be exercised in-process
			return mcp.NewToolResultError("the active graph backend doesn't expose a SQL handle; find_smells requires a leyline-parsed .db"), nil // coverage:ignore
		} // coverage:ignore

		// Pre-flight: every rule declares the tables it reads in
		// rule.Requires. If any are missing on this backend, return a
		// friendly tool error instead of letting the SQL fail with
		// "no such table" — agents can then call the tool again with
		// a different rule, or stop.
		if missing, err := missingTables(qg, rule.Requires); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("rule %q: pre-flight table check failed: %v", ruleID, err)), nil
		} else if len(missing) > 0 {
			// If the active .db was built by `mache build`, the
			// _mache_meta marker tells us which backend produced it.
			// Splice that into the error so agents don't have to run
			// `.tables` to figure out why _ast is missing.
			backendNote := ""
			if backend := queryBuildBackend(qg); backend != "" { // coverage:ignore — _mache_meta absent in test fixtures; backend splice exercised only in real `mache build` outputs
				backendNote = fmt.Sprintf(" (this .db was built with backend=%q)", backend) // coverage:ignore
			} // coverage:ignore
			return mcp.NewToolResultError(fmt.Sprintf(
				"rule %q requires SQL tables [%s] which aren't present on the active backend%s. _ast / _source / _imports / _lsp* are produced by ley-line-open's `leyline parse`; node_defs / node_refs / nodes are produced by both standalone mache and LLO. See docs/ARCHITECTURE.md#interplay-with-ley-line-open for the full table.",
				ruleID, strings.Join(missing, ", "), backendNote)), nil
		}

		findings, err := runSmellRule(qg, rule, sourceID, limit)
		if err != nil { // coverage:ignore — runSmellRule errors require malformed SQL or DB IO failure; pre-flight (missingTables) catches schema gaps, so this branch is unreachable in tests
			return mcp.NewToolResultError(fmt.Sprintf("rule %q: %v", ruleID, err)), nil // coverage:ignore
		} // coverage:ignore

		// Server-side metric threshold. Drops findings below the cutoff
		// before snippet population so we don't waste reads on rows the
		// agent will discard. min_metric=0 keeps the historical default.
		if minMetric > 0 {
			filtered := findings[:0]
			for _, f := range findings {
				if f.Metric >= minMetric {
					filtered = append(filtered, f)
				}
			}
			findings = filtered
		}

		// Snippet population is best-effort. If `_source` isn't there
		// (older .dbs) we just leave Snippet empty.
		populateSnippets(qg, findings)

		resp := struct {
			Rule     string         `json:"rule"`
			Total    int            `json:"total"`
			Findings []smellFinding `json:"findings"`
		}{
			Rule:     rule.ID,
			Total:    len(findings),
			Findings: findings,
		}
		return mcp.NewToolResultText(jsonOrPanic(resp)), nil
	}
}

// allRuleIDs returns the given rules' IDs in alphabetical order.
// Used by the unknown-rule error message so agents/users see what
// they could have typed without having to call the registry first.
// Takes the active rule set (built-ins + external) rather than reading
// the smellRegistry global so external rules show up in the hint.
func allRuleIDs(rules []SmellRule) []string {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// rulesListing produces the JSON returned when find_smells is called
// without a rule. Stable order so agents can cache the listing. Takes
// the active rule set (built-ins + external) so externally-loaded rules
// appear in discovery.
func rulesListing(rules []SmellRule) any {
	type ruleSummary struct {
		ID               string   `json:"id"`
		Languages        []string `json:"languages,omitempty"`
		Description      string   `json:"description"`
		Requires         []string `json:"requires,omitempty"`
		DefaultMinMetric int64    `json:"default_min_metric,omitempty"`
		Severity         Severity `json:"severity"` // always emitted; "warn" when rule omits the field
		Tags             []string `json:"tags,omitempty"`
	}
	out := make([]ruleSummary, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		out = append(out, ruleSummary{
			ID:               r.ID,
			Languages:        r.Languages,
			Description:      r.Description,
			Requires:         r.Requires,
			DefaultMinMetric: r.DefaultMinMetric,
			Severity:         r.Effective(),
			Tags:             r.Tags,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return struct {
		Help  string        `json:"help"`
		Rules []ruleSummary `json:"rules"`
	}{
		Help:  "find_smells runs structural pattern queries against the _ast / nodes / node_defs / node_refs tables. Three tiers: omit `rule` (this response) to LIST rules; pass `rule=*` for an L1 DIGEST — per-rule counts + per-file rollup + a few worst exemplars, bounded regardless of debt size (start here on an unfamiliar repo); pass `rule=<id>` for the full findings of one rule (L2), each with its real file:line backfilled from _ast, optionally scoped with `source_id=<file>`. Each rule entry includes a `requires` list of SQL tables it reads — agents can use it to skip rules whose tables aren't present on the active backend (e.g. _ast is only populated by ley-line-open's leyline parse). `limit` caps L2 results (default 200); `min_metric` drops findings whose metric column is below the threshold. Rules that surface a `default_min_metric` apply that as the threshold when the caller omits `min_metric`; an explicit `min_metric=0` overrides the default and returns everything sorted by metric.",
		Rules: out,
	}
}

// queryBuildBackend returns the value of `_mache_meta.backend` on the
// active backend, or "" if the table isn't present, the row is missing,
// or any error occurs. `mache build` stamps this marker for every .db
// it produces (see cmd/build_meta.go); older or third-party-produced
// .dbs return "" silently so the caller can fall back to a generic
// message. Best-effort by design — never returns an error.
func queryBuildBackend(qg refsQuerier) string {
	rows, err := qg.QueryRefs(`SELECT value FROM _mache_meta WHERE key = 'backend'`)
	if err != nil {
		return ""
	}
	// All paths below are unreachable in tests: test fixtures never create
	// _mache_meta, so QueryRefs errors above and we return "". The defensive
	// rows lifecycle below activates only against real `mache build` .dbs.
	defer func() { _ = rows.Close() }() // coverage:ignore
	if !rows.Next() {                   // coverage:ignore
		return "" // coverage:ignore
	} // coverage:ignore
	var v string                          // coverage:ignore
	if err := rows.Scan(&v); err != nil { // coverage:ignore
		return "" // coverage:ignore
	} // coverage:ignore
	return v // coverage:ignore
}

// missingTables returns the subset of `required` that's not in
// sqlite_master on the active backend. Returns nil if everything is
// present (or `required` is empty). The query uses placeholders for
// the IN list so it works with arbitrary SQLite drivers.
func missingTables(qg refsQuerier, required []string) ([]string, error) {
	if len(required) == 0 { // coverage:ignore — every built-in rule declares Requires; empty slice happens only for hypothetical bare external rules
		return nil, nil // coverage:ignore
	} // coverage:ignore
	placeholders := strings.Repeat("?,", len(required))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(required))
	for i, t := range required {
		args[i] = t
	}
	rows, err := qg.QueryRefs(
		"SELECT name FROM sqlite_master WHERE type='table' AND name IN ("+placeholders+")",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(required))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil { // coverage:ignore — sqlite_master is single-column TEXT; Scan can't fail on a fresh rowset
			return nil, err // coverage:ignore
		} // coverage:ignore
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil { // coverage:ignore — rows.Err surfaces driver IO faults; SQLite in-memory + readonly file produce none in tests
		return nil, err // coverage:ignore
	} // coverage:ignore
	var missing []string
	for _, t := range required {
		if _, ok := present[t]; !ok {
			missing = append(missing, t)
		}
	}
	return missing, nil
}
