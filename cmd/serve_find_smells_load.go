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
// "the third rule errored, what was the first?" debugging). Files
// that fail to parse log a warning and are skipped; mache still
// starts so a typo in an external rule can't take down the server.
const SmellRulesEnvVar = "MACHE_SMELL_RULES_DIR"

// LoadExternalSmellRules reads `*.json` files from dir, parses each
// as a SmellRule, validates the result, and returns the parsed rules.
// On a per-file error returns the error so the caller can decide:
// init() logs and skips, tests fail loudly.
//
// Validation:
//   - ID must be non-empty and not collide with a built-in rule
//   - Query must be non-empty and contain exactly one '%s'
//     placeholder for the scope clause (matches the built-in contract)
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

	builtin := builtinRuleIDs()
	seen := make(map[string]string, len(builtin))
	for id := range builtin {
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
	return nil
}

// builtinRuleIDs returns a set of IDs from the hard-coded
// smellRegistry. Used for collision detection during external load.
func builtinRuleIDs() map[string]struct{} {
	ids := make(map[string]struct{}, len(smellRegistry))
	for _, r := range smellRegistry {
		ids[r.ID] = struct{}{}
	}
	return ids
}
