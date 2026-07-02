package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionsPinScript locates scripts/actions-pin-lint.sh by walking up to the
// module root. Bead mache-b8900d.
func actionsPinScript(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "scripts", "actions-pin-lint.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

func TestActionsPinLint_FlagsUnpinnedRef(t *testing.T) {
	dir := t.TempDir()
	// One unpinned tag, one properly SHA-pinned, one exempt local ref, one
	// exempt docker ref.
	wf := "jobs:\n  a:\n    steps:\n" +
		"      - uses: actions/checkout@v4\n" +
		"      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6\n" +
		"      - uses: ./.github/actions/local\n" +
		"      - uses: docker://alpine:3.19\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wf.yml"), []byte(wf), 0o644))

	out, err := exec.Command("bash", actionsPinScript(t), dir).CombinedOutput()
	require.Error(t, err, "must exit non-zero when an unpinned ref is present\n%s", out)
	assert.Contains(t, string(out), "UNPINNED")
	assert.Contains(t, string(out), "actions/checkout@v4", "bare tag must be flagged")
	assert.Contains(t, string(out), "docker://alpine:3.19", "mutable docker tag must be flagged")
	assert.NotContains(t, string(out), "setup-go", "SHA-pinned ref must not be flagged")
	assert.NotContains(t, string(out), "actions/local", "local ./ ref is exempt")
}

func TestActionsPinLint_PassesWhenAllPinned(t *testing.T) {
	dir := t.TempDir()
	upperSHA := strings.Repeat("A", 40)     // uppercase 40-hex must be accepted
	dockerDigest := strings.Repeat("a", 64) // docker digest pin
	wf := "jobs:\n  a:\n    steps:\n" +
		"      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0\n" +
		"      - uses: actions/foo@" + upperSHA + "\n" +
		"      - uses: docker://alpine@sha256:" + dockerDigest + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wf.yml"), []byte(wf), 0o644))

	out, err := exec.Command("bash", actionsPinScript(t), dir).CombinedOutput()
	require.NoError(t, err, "must exit zero when all refs are SHA-pinned (incl. uppercase SHA + docker digest); got:\n%s", out)
	assert.Contains(t, string(out), "SHA-pinned")
}

// TestActionsPinLint_RepoIsClean is the live guard: every workflow ref in this
// repo must be SHA-pinned. Mirrors what CI runs via `task actions:lint`, so a
// regression fails `go test ./cmd/` too.
func TestActionsPinLint_RepoIsClean(t *testing.T) {
	out, err := exec.Command("bash", actionsPinScript(t)).CombinedOutput()
	assert.NoError(t, err, "repo has unpinned workflow refs:\n%s", out)
}
