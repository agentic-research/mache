package cmd

import (
	"log"
	"os"
	"strings"
)

func init() {
	// Append external rules from $MACHE_SMELL_RULES_DIR so they
	// share the registry with built-ins. Delegates to
	// appendExternalRulesFromEnv so the wiring is testable —
	// init() itself can't be re-run with different env values
	// across tests, but the helper takes the value as a param.
	// Errors are already logged inside the helper; init() just
	// drops the return value.
	_, _ = appendExternalRulesFromEnv(os.Getenv(SmellRulesEnvVar))
}

// appendExternalRulesFromEnv loads external rules from the given
// directory path (typically `$MACHE_SMELL_RULES_DIR`) and appends
// them to smellRegistry. A load error logs and skips the
// extension entirely — mache stays up so a typo in an external
// rule can't take down the server. Tests call
// LoadExternalSmellRules directly to assert errors loudly; this
// helper exercises the full append + log path.
//
// Returns (added, err): the count of appended rules and any
// load error. err is nil-on-empty-dir (no env, no error). Tests
// inspect the return; init() ignores it (logs at the source).
func appendExternalRulesFromEnv(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	rules, err := LoadExternalSmellRules(dir)
	if err != nil {
		log.Printf("smell rules: skipping external rules in %s: %v", dir, err)
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}
	smellRegistry = append(smellRegistry, rules...)
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	log.Printf("smell rules: loaded %d external rule(s) from %s: %s",
		len(rules), dir, strings.Join(ids, ", "))
	return len(rules), nil
}
