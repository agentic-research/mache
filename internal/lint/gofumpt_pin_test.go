package lint

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-task/task/v3/taskfile/ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// gofumptPin is how every gofumpt invocation must run: the version pinned by
// the Taskfile's GOFUMPT_VERSION, never whatever `gofumpt` is on PATH. A
// Homebrew upgrade to gofumpt 0.12.0 made `task fmt:check` disagree with CI
// on dozens of untouched files, because the task installed v0.9.2 into
// GOPATH/bin and then ran the `gofumpt` PATH found first (mache-198250).
const gofumptPin = "go run mvdan.cc/gofumpt@{{.GOFUMPT_VERSION}}"

// TestGofumptRunsThePinnedVersion: no Taskfile command and no pre-commit hook
// invokes a bare `gofumpt`, and fmt:check runs the pinned one.
func TestGofumptRunsThePinnedVersion(t *testing.T) {
	root := filepath.Join("..", "..")

	data, err := os.ReadFile(filepath.Join(root, "Taskfile.yml"))
	require.NoError(t, err)
	var tf ast.Taskfile
	require.NoError(t, yaml.Unmarshal(data, &tf))
	pinnedInCheck := false
	for name, task := range tf.Tasks.All(nil) {
		for _, c := range task.Cmds {
			assert.NotContains(t, externalBinaries(c.Cmd), "gofumpt",
				"task %s runs the gofumpt on PATH; use %q", name, gofumptPin)
			if name == "fmt:check" && strings.Contains(c.Cmd, gofumptPin) {
				pinnedInCheck = true
			}
		}
	}
	assert.True(t, pinnedInCheck, "fmt:check must run %q", gofumptPin)

	var precommit struct {
		Repos []struct {
			Hooks []struct {
				ID    string `yaml:"id"`
				Entry string `yaml:"entry"`
			} `yaml:"hooks"`
		} `yaml:"repos"`
	}
	data, err = os.ReadFile(filepath.Join(root, ".pre-commit-config.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, &precommit))
	hooks := 0
	for _, r := range precommit.Repos {
		for _, h := range r.Hooks {
			hooks++
			assert.False(t, slices.Contains(externalBinaries(h.Entry), "gofumpt"),
				"pre-commit hook %s runs the gofumpt on PATH; run it through the Taskfile", h.ID)
		}
	}
	require.Positive(t, hooks, "parsed no pre-commit hooks")
}
