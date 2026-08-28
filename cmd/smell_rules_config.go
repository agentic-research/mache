package cmd

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/internal/projcfg"
)

// resolveSmellRulesDir picks the external smell-rules directory for the
// current invocation using a fixed precedence:
//
//	explicit flag > MACHE_SMELL_RULES_DIR env > .mache.json smellRulesDir > ""
//
// Flag and env values are operator-supplied at invocation time and are
// used verbatim (the operator points them wherever they like). A
// smellRulesDir read from .mache.json is treated as project-relative:
// if it's a relative path it's joined onto projectDir (the directory
// that holds the .mache.json) and containment-checked with the same
// guard resolveDataSource uses, so an untrusted config can't escape the
// project tree via `../`. An empty string means "built-in rules only".
func resolveSmellRulesDir(flagVal, projectDir string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(SmellRulesEnvVar)); v != "" {
		return v
	}
	// .mache.json is optional; any load error (missing file, empty
	// sources, bad JSON) simply means "no configured rules dir".
	cfg, err := projcfg.LoadProjectConfig(projectDir)
	if err != nil || cfg == nil {
		return ""
	}
	dir := strings.TrimSpace(cfg.SmellRulesDir)
	if dir == "" {
		return ""
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	resolved := filepath.Join(projectDir, dir)
	if err := projcfg.CheckPathContainment(resolved, projectDir); err != nil {
		log.Printf("smell rules: ignoring smellRulesDir %q from %s: %v", dir, projcfg.ConfigFileName, err)
		return ""
	}
	return resolved
}

// activeSmellRules returns the rule set for a single find_smells
// invocation: a fresh copy of the built-in registry plus any external
// rules loaded from dir. It is called PER REQUEST (not once at package
// init) so a rule JSON dropped into dir becomes visible on the next
// call with no restart and no reconnect — the live-reload property.
//
// The returned slice is always a distinct copy of smellRegistry, never
// the package global, so callers can hand out pointers into it without
// aliasing the built-in base.
//
// On an external-load error the built-ins are returned alongside the
// error: the CLI surfaces the error (a bad rule file should fail the
// run loudly), while serve logs it and falls back to the built-ins so a
// typo in an external rule can't take the daemon down — the same safety
// property the old init()-time load guaranteed.
func activeSmellRules(dir string) ([]SmellRule, error) {
	rules := make([]SmellRule, len(smellRegistry))
	copy(rules, smellRegistry)
	external, err := LoadExternalSmellRules(dir)
	if err != nil {
		return rules, err
	}
	return append(rules, external...), nil
}
