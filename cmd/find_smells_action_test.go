package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionYAMLPath locates .github/actions/find-smells/action.yml from the
// module root.
func actionYAMLPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".github", "actions", "find-smells", "action.yml")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// TestFindSmellsAction_Contract asserts the composite action exists, is a
// composite action, and its shell steps only reference declared inputs and
// the real find-smells subcommands. Cheap guard against drift between the
// CLI and the action wiring.
func TestFindSmellsAction_Contract(t *testing.T) {
	body, err := os.ReadFile(actionYAMLPath(t))
	require.NoError(t, err, "action.yml must exist")
	src := string(body)

	assert.Contains(t, src, "using: 'composite'")
	// Declared inputs.
	for _, in := range []string{"mache-version", "schema", "baseline", "fail-on-new", "upload-sarif"} {
		assert.Contains(t, src, in+":", "input %q must be declared", in)
	}
	// No undeclared inputs referenced.
	declared := map[string]bool{
		"mache-version": true, "schema": true, "baseline": true,
		"fail-on-new": true, "upload-sarif": true,
	}
	for _, line := range strings.Split(src, "\n") {
		idx := strings.Index(line, "inputs.")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("inputs."):]
		name := strings.FieldsFunc(rest, func(r rune) bool {
			return !(r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		})[0]
		assert.Truef(t, declared[name], "action references undeclared input %q", name)
	}
	// The action must drive the real CLI surface.
	assert.Contains(t, src, "mache build")
	assert.Contains(t, src, "--format sarif")
	assert.Contains(t, src, "--write-baseline")
	assert.Contains(t, src, "--baseline ")
}
