package lint

// The cmd-decomposition invariants (mache-96c378, stage 8). The eight
// extracted packages form a DAG that took eight PRs to carve; these tests are
// what keeps a well-meaning future edit from quietly growing it back.
// Wired into `task test` (this package) → `task ci` → the pre-push hook and CI.

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const module = "github.com/agentic-research/mache"

// decompPackages is the extracted eight, with each one's ALLOWED internal
// dependencies among the eight (the declared DAG). Any new edge fails here
// until this table is deliberately amended in review.
var decompPackages = map[string][]string{
	"internal/testutil":     {},
	"internal/projcfg":      {},
	"internal/mountmeta":    {},
	"internal/smells":       {"internal/projcfg"},
	"internal/schemainfer":  {"internal/leylinegraph"},
	"internal/leylinegraph": {},
	"internal/buildcache":   {"internal/projcfg"},
	// daemonguard keeps per-machine daemon state under ~/.mache, resolved
	// through projcfg's home seam so it inherits the test-hermeticity guard
	// (mache-956488 / mache-3e78d2).
	"internal/daemonguard": {"internal/projcfg"},
	"internal/mcpserve": {
		"internal/projcfg", "internal/mountmeta", "internal/smells",
		"internal/schemainfer", "internal/leylinegraph", "internal/daemonguard",
	},
}

// goListDeps returns the module-internal transitive deps of pkg.
func goListDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", module+"/"+pkg).Output()
	require.NoErrorf(t, err, "go list -deps %s", pkg)
	deps := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rest, ok := strings.CutPrefix(line, module+"/"); ok && rest != pkg {
			deps[rest] = true
		}
	}
	return deps
}

// TestDecomp_NoPackageImportsCmd is invariant 1a: the arrow between cmd and
// the extracted packages points ONE way. cmd wires (register.go, injection);
// the packages never look up. A single upward import re-creates the
// cycle-by-construction the four symbol re-homes (R1–R4) existed to kill.
func TestDecomp_NoPackageImportsCmd(t *testing.T) {
	for pkg := range decompPackages {
		if goListDeps(t, pkg)["cmd"] {
			t.Errorf("%s imports %s/cmd — the decomposition arrow points one way", pkg, module)
		}
	}
}

// TestDecomp_DAGMatchesDeclaredEdges is invariant 1b: each extracted
// package's dependencies AMONG THE EIGHT are exactly a subset of the
// declared DAG. Growing an edge is a design decision, not a convenience —
// amend decompPackages in the same PR and say why.
func TestDecomp_DAGMatchesDeclaredEdges(t *testing.T) {
	for pkg, allowed := range decompPackages {
		allowedSet := map[string]bool{}
		for _, a := range allowed {
			allowedSet[a] = true
		}
		deps := goListDeps(t, pkg)
		var illegal []string
		for other := range decompPackages {
			if other != pkg && deps[other] && !allowedSet[other] {
				illegal = append(illegal, other)
			}
		}
		sort.Strings(illegal)
		assert.Emptyf(t, illegal,
			"%s imports undeclared decomposition packages %v — amend the DAG table deliberately or break the edge",
			pkg, illegal)
	}
}

// TestDecomp_CmdSizeRatchet is invariant 2: cmd/ may only SHRINK. The
// post-stage-8 count is the grandfathered ceiling; if a legitimate new
// command file raises it, lower the slack elsewhere or amend with review.
func TestDecomp_CmdSizeRatchet(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{range .GoFiles}}{{.}}\n{{end}}", module+"/cmd").Output()
	require.NoError(t, err)
	files := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files++
		}
	}
	const ceiling = 17 // post-stage-8 prod-file count; may only decrease
	assert.LessOrEqualf(t, files, ceiling,
		"cmd/ grew to %d prod files (ceiling %d) — new capability belongs in an internal package, wired via cmd/register.go",
		files, ceiling)
}

// TestDecomp_LdflagsTargetsStayInCmd is invariant 4: the three -X targets
// baked into Taskfile.yml and release.yml must keep resolving to package cmd
// declarations, or every release builds with a silent default identity.
// Checked by parsing the package source (go doc elides grouped var blocks).
func TestDecomp_LdflagsTargetsStayInCmd(t *testing.T) {
	dirOut, err := exec.Command("go", "list", "-f", "{{.Dir}}", module+"/cmd").Output()
	require.NoError(t, err)
	dir := strings.TrimSpace(string(dirOut))
	for _, decl := range []string{"Commit ", "Date ", "var buildVersion"} {
		found, gerr := exec.Command("grep", "-rl", decl, "--include=*.go", dir).Output()
		require.NoErrorf(t, gerr,
			"cmd.%s (an ldflags -X target in Taskfile.yml/release.yml) is no longer declared in package cmd",
			strings.TrimSpace(strings.TrimPrefix(decl, "var ")))
		assert.NotEmpty(t, strings.TrimSpace(string(found)))
	}
}

// TestDecomp_EmbedPatternsResolve is invariant 6: every //go:embed pattern in
// the repo must match at least one file. A moved-but-empty embed dir compiles
// fine and ships a binary with zero rules — the silent stale-binary factory
// class (mache-46af85), which the smells asset move made reachable.
func TestDecomp_EmbedPatternsResolve(t *testing.T) {
	root := moduleRoot(t)
	out, err := exec.Command("grep", "-rn", "--include=*.go", "^//go:embed", root).Output()
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.NotEmpty(t, lines, "the repo has embeds; finding none means the grep broke")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 || strings.Contains(parts[0], "testdata") {
			continue
		}
		dir := filepath.Dir(parts[0])
		for _, pattern := range strings.Fields(strings.TrimPrefix(parts[2], "//go:embed")) {
			matches, gerr := filepath.Glob(filepath.Join(dir, pattern))
			require.NoErrorf(t, gerr, "bad embed pattern %q in %s", pattern, parts[0])
			assert.NotEmptyf(t, matches,
				"//go:embed %s in %s matches NO files — an empty embed compiles and ships a hollow binary",
				pattern, parts[0])
		}
	}
}
