package cmd

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// SmellRule describes one structural code-smell pattern. Rules are
// language-aware: each carries the language(s) it applies to plus a
// SQL query template that runs against the `_ast` / `nodes` /
// `node_defs` / `node_refs` tables produced by `leyline parse`.
//
// The query MUST select exactly seven columns:
//
//	source_id, node_id, start_byte, end_byte, start_row, start_col, metric
//
// in that order. `metric` is an integer score (cyclomatic complexity,
// fan-out count, line length, etc.); binary rules that don't carry
// a metric emit `0 AS metric`. The handler shapes the row into a
// uniform response with 1-based line/column and an optional snippet.
//
// `ScopeColumn` is the SQL expression the handler will compare to
// source_id when the caller passes `source_id` to find_smells (e.g.
// "lit.source_id" for AST-walking rules; "n.source_file" for rules
// that join via nodes). The query template MUST contain a single
// `%s` placeholder where the scope clause should be spliced in;
// rules unconcerned with scoping can leave ScopeColumn blank.
//
// Threshold filtering on the metric column is server-side via the
// `min_metric` request arg — handler drops findings whose metric
// is below the cutoff before returning. Default 0 keeps every row.
//
// Bead mache-6z2e tracks the broader "machelint" idea — declarative
// rules consumed via this tool. Today the registry is hard-coded; in
// the future it becomes user-extensible (declarative JSON, per-repo
// overrides, etc.).
type SmellRule struct {
	ID          string   // stable identifier, used as the MCP tool argument
	Languages   []string // matches `_source.language` values; empty = any
	Description string   // shown in the help payload
	Query       string   // SQL with one `%s` placeholder for the optional scope clause
	ScopeColumn string   // SQL expression to compare to source_id; empty disables source_id scoping
	// Requires lists the SQL tables this rule reads. Surfaced in the
	// rules listing so an agent can decide upfront whether the rule is
	// usable on the active backend. Standalone mache emits `nodes`,
	// `node_refs`, `node_defs`; LLO-built .db additionally has `_ast`,
	// `_source`, `_imports`, `_lsp*`. See docs/ARCHITECTURE.md
	// "Interplay with ley-line-open" for the full table.
	Requires []string
	// DefaultMinMetric is the rule's recommended metric threshold
	// when the request omits min_metric. Zero means "no default
	// threshold; return all rows." Used for metric-bearing rules
	// where the query's natural output includes a long tail of
	// uninteresting low-metric rows (e.g. long_function returns
	// every function regardless of size; without a default
	// threshold the response is dominated by 1-line setters).
	// Callers can pass min_metric=0 explicitly to override the
	// default and see everything; any non-zero min_metric also
	// overrides.
	DefaultMinMetric int64
	// Severity controls whether a finding from this rule is fatal to
	// the gate. Three tiers per ESLint precedent (off/warn/error) —
	// see ADR-0018 and bead mache-ec1a06 for the prior-art rationale.
	//
	//   - "off"   — rule defined but never produces findings; useful
	//     for shipping a rule in a pack that's disabled for now
	//   - "warn"  — emits findings, exit 0 (observability default)
	//   - "error" — emits findings, exit 1 (rule author asserts this
	//     drift must not ship)
	//
	// Default ("") is treated as "warn" — preserves the existing
	// observability contract for all rules that haven't opted in.
	// Gate decision is made at CLI invocation via --fail-on (pylint
	// precedent): rule says what's true, gate decides whether truth
	// is fatal.
	Severity Severity
	// Tags is a free-form classification used for CLI selection via
	// --tags=foo,bar. Per ADR-0018 + research, NOT a closed enum:
	// stages (pre-commit, ci) emerge as CLI profiles from
	// (--tags × --fail-on) combinations, not from a fixed schema.
	// Cap at 3-5 tags per rule to avoid the clippy-group explosion;
	// expand the vocabulary deliberately, not reactively.
	Tags []string
}

// Severity is one of off / warn / error. ESLint precedent (string
// names, no numeric aliases). See mache-ec1a06 for the prior-art
// rationale across ruff/pylint/eslint/clippy/semgrep.
type Severity string

const (
	SeverityOff   Severity = "off"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Effective returns the resolved severity, treating the zero value
// ("") as warn — the default for any rule that hasn't opted in.
// Callers asking "is this rule fatal" should always go through
// Effective() rather than reading Severity directly.
func (r *SmellRule) Effective() Severity {
	switch r.Severity {
	case SeverityOff, SeverityError:
		return r.Severity
	default:
		return SeverityWarn
	}
}

// smellRegistry holds the BUILT-IN rules only. External rules from the
// resolved smell-rules dir (flag / $MACHE_SMELL_RULES_DIR / .mache.json
// smellRulesDir) are merged with a copy of this slice per find_smells
// request in activeSmellRules — never appended to this global — so they
// participate in discovery, listing, and dispatch the same way while
// remaining live-reloadable.
//
// The built-ins are shipped as embedded JSON (cmd/rules/*.json, one
// SmellRule per file) and loaded at package init via
// mustLoadEmbeddedRules. They use the SAME JSON DSL and the SAME
// parse+validate path as external rules (see LoadExternalSmellRules)
// — the only difference is provenance: embedded rules are compiled
// into the binary, so a malformed one is a build-time bug and panics
// rather than being skipped. Keeping the builtins as data (not Go
// struct literals) means the registry, the external-rules dir, and
// the authoring guide all speak one format.
var smellRegistry = mustLoadEmbeddedRules()

//go:embed rules/*.json
var embeddedRulesFS embed.FS

// mustLoadEmbeddedRules parses every cmd/rules/*.json file into a
// SmellRule, validates it with the same validateSmellRule contract
// external rules must satisfy, and returns them in deterministic
// order (sorted by filename). It panics on any read / parse /
// validation error: embedded rules are compiled into the binary, so
// a bad one is a programming error that must fail the build, never a
// runtime surprise. Determinism matters because rule order is
// user-visible (listing / help output) and keeps tests stable.
func mustLoadEmbeddedRules() []SmellRule {
	rules, err := loadRuleFS(embeddedRulesFS, "rules")
	if err != nil {
		panic(fmt.Sprintf("mache: embedded smell rules are invalid (build-time bug): %v", err))
	}
	if len(rules) == 0 {
		panic("mache: no embedded smell rules found under rules/*.json")
	}
	return rules
}

// loadRuleFS reads every *.json file directly under dir in the given
// filesystem, parses+validates each as a SmellRule, and returns them
// sorted by filename. Shared by the embedded-rules loader; mirrors the
// os.ReadDir path in LoadExternalSmellRules so both sources honor the
// identical parse/validate/ordering contract.
func loadRuleFS(fsys fs.FS, dir string) ([]SmellRule, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	rules := make([]SmellRule, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		path := dir + "/" + name
		raw, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		rule, err := parseSmellRuleJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if origin, exists := seen[rule.ID]; exists {
			return nil, fmt.Errorf("%s: rule ID %q already defined (%s)", path, rule.ID, origin)
		}
		seen[rule.ID] = path
		rules = append(rules, rule)
	}
	return rules, nil
}

// parseSmellRuleJSON unmarshals a single SmellRule JSON object and
// runs the shared validation contract. Extracted so the embedded
// loader and the external-dir loader parse+validate identically.
func parseSmellRuleJSON(raw []byte) (SmellRule, error) {
	var rule SmellRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return SmellRule{}, fmt.Errorf("parse: %w", err)
	}
	if err := validateSmellRule(rule); err != nil {
		return SmellRule{}, err
	}
	return rule, nil
}
