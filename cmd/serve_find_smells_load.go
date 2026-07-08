package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SmellRulesEnvVar names the environment variable that points to a
// directory of JSON files containing additional SmellRule entries.
// External rules are merged with the built-in registry per find_smells
// request (see activeSmellRules) and are otherwise indistinguishable
// from built-ins — same MCP shape, same query contract, same pre-flight
// table check. Loading per-request (rather than once at package init)
// is what makes rule files live-reloadable: drop a new JSON in the dir
// and the next call sees it, no restart.
//
// Each file in the directory must be a single SmellRule object
// (not an array — one rule per file keeps diffs clean and avoids
// "the third rule errored, what was the first?" debugging). The
// loader is fail-fast: a single read / parse / validation /
// collision error aborts loading, returning the error to the
// caller. serve logs and falls back to built-ins in that case so a
// typo can't take down the server; the CLI surfaces the error
// directly.
const SmellRulesEnvVar = "MACHE_SMELL_RULES_DIR"

// builtinSmellRuleIDs is a snapshot of the rule IDs in
// smellRegistry as it stood at package load. smellRegistry now holds
// ONLY the built-ins (externals are merged per-request in
// activeSmellRules, never appended to the global), so this snapshot
// equals the built-in set for the process lifetime. It's used by
// LoadExternalSmellRules for collision detection so the "this collides
// with a built-in" message stays accurate across repeated per-request
// loads.
var builtinSmellRuleIDs = func() map[string]struct{} {
	ids := make(map[string]struct{}, len(smellRegistry))
	for _, r := range smellRegistry {
		ids[r.ID] = struct{}{}
	}
	return ids
}()

// LoadExternalSmellRules reads `*.json` files from dir, parses each
// as a SmellRule, validates the result, and returns the parsed rules.
// Fail-fast: any read, parse, validation, or collision error aborts
// and returns immediately — caller decides whether to log+skip or
// treat as fatal. init() logs and skips; tests fail loudly.
//
// Validation:
//   - ID must be non-empty and not collide with a built-in rule
//     (or another external loaded from this same dir)
//   - Query must be non-empty, contain exactly one '%s' placeholder
//     for the scope clause, AND be a valid fmt.Sprintf format string
//     (no stray `%` chars that fmt would treat as verbs — common
//     trap with SQL `LIKE '%foo%'` patterns; escape as `%%`)
//   - Requires may be empty (rule reads no tables) or list table names
func LoadExternalSmellRules(dir string) ([]SmellRule, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	// Build the seen set from the snapshot, not from the live
	// smellRegistry. If init() has already appended externals,
	// reading from smellRegistry directly would label them as
	// "built-in" in collision errors — misleading.
	seen := make(map[string]string, len(builtinSmellRuleIDs))
	for id := range builtinSmellRuleIDs {
		seen[id] = "built-in"
	}

	var rules []SmellRule
	// Sort for deterministic load order — keeps test golden values
	// stable when files are added.
	files := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

	for _, e := range files {
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
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

// validateSmellRule enforces the contract every SmellRule must
// satisfy to be safely added to the registry. Mirrors what the
// built-in registry implicitly guarantees by being hand-written.
//
// The fmt.Sprintf check catches a common trap: SQL LIKE patterns
// like `LIKE '%foo%'` need their `%` chars escaped as `%%` because
// runSmellRule splices the scope clause via fmt.Sprintf(query, scope).
// An unescaped `%f` would be treated as the float verb and produce
// `%!f(string=...)` corruption at runtime. Reject at load time.
//
// ScopeColumn is interpolated unescaped into the SQL (runSmellRule
// builds `"AND " + rule.ScopeColumn + " = ?"`), so an external rule
// could in principle break out of the scope clause with `;`, `--`,
// or unexpected characters. The trust boundary here is whoever
// controls $MACHE_SMELL_RULES_DIR (operator), so this is defense in
// depth rather than a vulnerability — but the cost is one regex-shaped
// check, and the value is "a typo in an external rule can't silently
// corrupt the query." The whitelist mirrors what the built-in
// ScopeColumn values use (identifiers, dots, COALESCE/NULLIF call
// shape, comma, paren, single quote, space).
func validateSmellRule(r SmellRule) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("rule ID is required")
	}
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("rule %q: Query is required", r.ID)
	}
	if strings.Count(r.Query, "%s") != 1 {
		return fmt.Errorf("rule %q: Query must contain exactly one '%%s' placeholder for the scope clause", r.ID)
	}
	// Format with an empty scope clause and check for fmt error
	// markers (`%!`) — flags any other unescaped `%` sequences.
	formatted := fmt.Sprintf(r.Query, "")
	if strings.Contains(formatted, "%!") {
		return fmt.Errorf("rule %q: Query has unescaped '%%' chars (other than the single '%%s' placeholder); SQL '%%' must be escaped as '%%%%' (e.g. LIKE '%%%%foo%%%%')", r.ID)
	}
	if err := validateScopeColumn(r.ScopeColumn); err != nil {
		return fmt.Errorf("rule %q: %w", r.ID, err)
	}
	return nil
}

// validateScopeColumn rejects ScopeColumn values that contain SQL
// statement terminators, comment sequences, or characters outside
// the small set the built-in registry uses. Empty is allowed (the
// rule opts out of source_id scoping). See validateSmellRule's
// comment for the threat model.
func validateScopeColumn(s string) error {
	if s == "" {
		return nil
	}
	if strings.Contains(s, ";") {
		return fmt.Errorf("ScopeColumn must not contain ';' (statement terminator)")
	}
	if strings.Contains(s, "--") {
		return fmt.Errorf("ScopeColumn must not contain '--' (SQL line comment)")
	}
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '_' || r == '.' || r == ',' || r == '(' || r == ')' || r == '\'' || r == ' ':
		default:
			return fmt.Errorf("ScopeColumn contains disallowed character %q; allowed: identifiers, '.', ',', '(', ')', \"'\", and space", r)
		}
	}
	return nil
}
