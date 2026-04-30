package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SmellRulesEnvVar names the environment variable that points to a
// directory of JSON files containing additional SmellRule entries.
// External rules are appended to the built-in registry at process
// start and are otherwise indistinguishable from built-ins — same
// MCP shape, same query contract, same pre-flight table check.
//
// Each file in the directory must be a single SmellRule object
// (not an array — one rule per file keeps diffs clean and avoids
// "the third rule errored, what was the first?" debugging). The
// loader is fail-fast: a single read / parse / validation /
// collision error aborts loading, returning the error to the
// caller. init() logs and skips all external rules in that case
// so a typo can't take down the server; tests assert the error
// directly.
const SmellRulesEnvVar = "MACHE_SMELL_RULES_DIR"

// builtinSmellRuleIDs is a snapshot of the rule IDs in
// smellRegistry as it stood at package load — captured before any
// external rules are appended. Used by LoadExternalSmellRules for
// collision detection so the "this collides with a built-in"
// message stays accurate even if external rules are loaded
// multiple times or after init() has mutated smellRegistry.
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
		var rule SmellRule
		if err := json.Unmarshal(raw, &rule); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := validateSmellRule(rule); err != nil {
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
	return nil
}
