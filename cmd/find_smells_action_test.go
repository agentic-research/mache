package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleRoot walks up from the test's working directory to the
// directory containing go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// actionYAMLPath locates .github/actions/find-smells/action.yml from the
// module root.
func actionYAMLPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(moduleRoot(t), ".github", "actions", "find-smells", "action.yml")
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
		isNameRune := func(r rune) bool {
			return r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		}
		end := strings.IndexFunc(rest, func(r rune) bool { return !isNameRune(r) })
		if end < 0 {
			end = len(rest)
		}
		name := rest[:end]
		assert.Truef(t, declared[name], "action references undeclared input %q", name)
	}
	// The action must drive the real CLI surface.
	assert.Contains(t, src, "mache build")
	assert.Contains(t, src, "--format sarif")
	assert.Contains(t, src, "--write-baseline")
	assert.Contains(t, src, "--baseline ")
}

// TestFindSmellsAction_TaskfileParity asserts the composite action's core
// gate invocation matches the Taskfile `smells` target's invocation shape
// (taskfile-ci-parity: the action is the CONSUMER-facing twin of the gate
// mache's own CI runs via `task smells`). It doesn't compare the scripts
// byte-for-byte — the action adds download/SARIF/summary steps by design —
// it pins the shared contract: same rule selection, same limit, same
// baseline-root portability trick, and the SAME documented default
// baseline path. Any deliberate divergence must update both files plus
// the note in .github/actions/find-smells/README.md ("Contract with
// mache's own gate").
func TestFindSmellsAction_TaskfileParity(t *testing.T) {
	root := moduleRoot(t)
	actionRaw, err := os.ReadFile(actionYAMLPath(t))
	require.NoError(t, err)
	taskRaw, err := os.ReadFile(filepath.Join(root, "Taskfile.yml"))
	require.NoError(t, err)
	action, taskfile := string(actionRaw), string(taskRaw)

	// The shared core invocation shape. Both gates must run every rule
	// (auto-skipping absent-table rules), uncapped in practice, with a
	// portable baseline root.
	const coreShape = "--rule '*' --limit 100000"
	assert.Contains(t, action, coreShape,
		"action must run the same rule-selection + limit shape as `task smells`")
	assert.Contains(t, taskfile, coreShape,
		"Taskfile smells gate must run the same rule-selection + limit shape as the action")
	assert.Contains(t, action, "--baseline-root")
	assert.Contains(t, taskfile, "--baseline-root")

	// The single documented default baseline path. The action input
	// default and the Taskfile gate must name the same file; the
	// consuming-repo docs (action README, examples/smell-rules/README.md)
	// reference this default rather than inventing their own.
	const defaultBaseline = "docs/smell-baseline.json"
	assert.Contains(t, action, "default: '"+defaultBaseline+"'",
		"action baseline input default must be the documented path")
	assert.Contains(t, taskfile, "--baseline "+defaultBaseline,
		"Taskfile smells gate must gate on the documented baseline path")

	// The action's default mache-version must be a well-formed release
	// tag at or above v0.13.0 — the floor its input description documents
	// (leyline auto-provisioning; below that the description's behavior
	// notes would be wrong for the default).
	m := regexp.MustCompile(`default: 'v(\d+)\.(\d+)\.\d+'`).FindStringSubmatch(action)
	require.NotNil(t, m, "action must declare a semver default for mache-version")
	major, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	minor, err := strconv.Atoi(m[2])
	require.NoError(t, err)
	assert.True(t, major > 0 || minor >= 13,
		"default mache-version %s predates v0.13.0 leyline auto-provisioning; the input description would be stale", m[0])
}
