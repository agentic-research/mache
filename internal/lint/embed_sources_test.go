package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/testutil"
)

// TestBuildSourcesCoverEveryEmbed enforces what the Taskfile's `build` task
// only asked for in a comment: every `//go:embed`ed asset must appear in
// `sources:`.
//
// Task's up-to-date check skips the rebuild when no listed source changed, so
// an unlisted embed means editing that asset leaves a STALE binary — one that
// still carries the old bytes while the tree says otherwise. mache-46af85
// recorded this once already, for the smell rules: the gate ran the stale
// binary and produced phantom "NEW findings".
//
// The comment did not prevent a recurrence. `schema/presets/*.json` was
// omitted, and editing a shipped preset schema silently projected with the old
// one — the edit appeared to do nothing, which is the most expensive shape a
// build bug can take. This test is that comment, enforced.
func TestBuildSourcesCoverEveryEmbed(t *testing.T) {
	root := testutil.MacheRepoRoot(t)

	sources := buildTaskSources(t, filepath.Join(root, "Taskfile.yml"))
	require.NotEmpty(t, sources, "could not parse the build task's sources: list")

	for _, e := range embedDirectives(t, root) {
		covered := false
		for _, src := range sources {
			if globCovers(src, e) {
				covered = true
				break
			}
		}
		assert.Truef(t, covered,
			"//go:embed %s is baked into the binary but matches no entry in the build task's "+
				"sources: — editing it will NOT trigger a rebuild, so the binary keeps the old "+
				"bytes while the tree says otherwise. Add a glob covering it.", e)
	}
}

// buildTaskSources extracts the `sources:` list from the `build:` task.
// Deliberately a small hand parser rather than a YAML dependency: the file is
// the CI contract, and a test that reads it the way a human does catches
// formatting the schema would silently accept.
func buildTaskSources(t *testing.T, taskfile string) []string {
	t.Helper()
	data, err := os.ReadFile(taskfile)
	require.NoError(t, err)

	var out []string
	inBuild, inSources := false, false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "  build:"):
			inBuild = true
		case inBuild && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.Contains(line, "build:"):
			// Next task at the same indent ends the build task.
			inBuild, inSources = false, false
		case inBuild && strings.TrimSpace(line) == "sources:":
			inSources = true
		case inSources:
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- ") {
				out = append(out, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"'`))
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				inSources = false
			}
		}
	}
	return out
}

// embedDirectives returns every //go:embed pattern in non-test Go files,
// resolved to a repo-relative path.
func embedDirectives(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/") {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		dir, rErr := filepath.Rel(root, filepath.Dir(path))
		if rErr != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			if pattern, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:embed "); ok {
				for _, p := range strings.Fields(pattern) {
					out = append(out, filepath.Join(dir, p))
				}
			}
		}
		return nil
	}))
	require.NotEmpty(t, out, "found no //go:embed directives — the walk is broken, not the repo")
	return out
}

// globCovers reports whether a sources: entry covers an embedded path. `**/*.go`
// covers Go files at any depth; otherwise the directory must match and the
// basename pattern must match.
func globCovers(source, embedded string) bool {
	if source == "**/*.go" {
		return strings.HasSuffix(embedded, ".go")
	}
	if source == embedded {
		return true
	}
	if filepath.Dir(source) != filepath.Dir(embedded) {
		return false
	}
	ok, err := filepath.Match(filepath.Base(source), filepath.Base(embedded))
	return err == nil && ok
}
