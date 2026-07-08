package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTaskfile writes body to a temp Taskfile.yml and returns its path.
func writeTaskfile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Taskfile.yml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// TestCheckTaskfile_UnguardedGate is the acceptance core: a gate task that
// invokes an external binary with NO guard yields exactly one gap.
func TestCheckTaskfile_UnguardedGate(t *testing.T) {
	tf := `version: '3'
tasks:
  verify:
    desc: Verify the release signature
    cmds:
      - cosign verify --key cosign.pub ghcr.io/acme/app:latest
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	require.Len(t, gaps, 1)
	assert.Equal(t, "verify", gaps[0].Task)
	assert.Equal(t, "cosign", gaps[0].Tool)
}

// TestCheckTaskfile_PreconditionGuard: a `preconditions: command -v cosign`
// entry satisfies the guard — zero gaps. Falsifiable pair with the above.
func TestCheckTaskfile_PreconditionGuard(t *testing.T) {
	tf := `version: '3'
tasks:
  verify:
    desc: Verify the release signature
    preconditions:
      - sh: command -v cosign
        msg: "cosign required"
    cmds:
      - cosign verify --key cosign.pub ghcr.io/acme/app:latest
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_InlineCommandVGuard: a flamegraphs-style inline
// `command -v` check in the cmds guards the binary — zero gaps.
func TestCheckTaskfile_InlineCommandVGuard(t *testing.T) {
	tf := `version: '3'
tasks:
  audit:
    desc: Supply-chain audit
    cmds:
      - |
        if ! command -v cargo-deny >/dev/null 2>&1; then
          echo "install cargo-deny: cargo install cargo-deny"
          exit 1
        fi
      - cargo-deny check
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_GoInstallGuard: a gofumpt-style `go install` in the same
// task provisions the tool — zero gaps.
func TestCheckTaskfile_GoInstallGuard(t *testing.T) {
	tf := `version: '3'
tasks:
  lint:
    desc: gofumpt check
    cmds:
      - go install mvdan.cc/gofumpt@v0.9.2
      - gofumpt -d -extra .
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_ArtifactExistenceGuard: the capnp false-PASS case — capnp
// can exit 0 on a plugin error, so a standalone `test -f <output>` assertion
// guards the tool. Zero gaps.
func TestCheckTaskfile_ArtifactExistenceGuard(t *testing.T) {
	tf := `version: '3'
tasks:
  build:
    desc: Generate capnp bindings
    cmds:
      - capnp compile -ogo schema.capnp
      - test -f schema.capnp.go
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_ConvenienceTaskSkipped: a non-gate (convenience) task is
// not analyzed, even when it invokes an unguarded external binary.
func TestCheckTaskfile_ConvenienceTaskSkipped(t *testing.T) {
	tf := `version: '3'
tasks:
  fetch-meta:
    desc: Pull metadata into a scratch file (best-effort)
    cmds:
      - jq '.name' package.json || true
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_TaskRefsAndPlatformCmdsSkipped: task-ref cmds (Cmd.Task set)
// and platform-scoped cmds (e.g. darwin-only codesign) are not treated as
// external-binary invocations.
func TestCheckTaskfile_TaskRefsAndPlatformCmdsSkipped(t *testing.T) {
	tf := `version: '3'
tasks:
  build:
    desc: Build and sign
    cmds:
      - go build -o bin/app .
      - cmd: codesign -s - bin/app
        platforms: [darwin]
  check:
    desc: Aggregate gate
    cmds:
      - task: build
`
	gaps, err := CheckTaskfile(writeTaskfile(t, tf))
	require.NoError(t, err)
	assert.Empty(t, gaps)
}

// TestCheckTaskfile_MultipleGapsSortedDeterministic: multiple unguarded tools
// across gate tasks are returned sorted by (Task, Tool), stably.
func TestCheckTaskfile_MultipleGapsSortedDeterministic(t *testing.T) {
	tf := `version: '3'
tasks:
  image:
    desc: Build OCI image
    cmds:
      - melange build melange.yaml
      - apko build apko.yaml app:latest app.tar
  release:
    desc: Sign and publish
    cmds:
      - cosign sign ghcr.io/acme/app:latest
`
	path := writeTaskfile(t, tf)
	first, err := CheckTaskfile(path)
	require.NoError(t, err)
	require.Len(t, first, 3)
	// Sorted by (Task, Tool): image/apko, image/melange, release/cosign.
	assert.Equal(t, "image", first[0].Task)
	assert.Equal(t, "apko", first[0].Tool)
	assert.Equal(t, "image", first[1].Task)
	assert.Equal(t, "melange", first[1].Tool)
	assert.Equal(t, "release", first[2].Task)
	assert.Equal(t, "cosign", first[2].Tool)

	// Determinism: repeated runs produce byte-identical ordering.
	for i := 0; i < 5; i++ {
		again, err := CheckTaskfile(path)
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

// TestCheckTaskfile_MissingFile surfaces a read error rather than panicking.
func TestCheckTaskfile_MissingFile(t *testing.T) {
	_, err := CheckTaskfile(filepath.Join(t.TempDir(), "nope.yml"))
	assert.Error(t, err)
}

// TestTaskfile_NoUnguardedGates locks the annotated real Taskfile at zero gaps.
// This catches future regressions where a new gate task adds an unguarded
// external binary.
func TestTaskfile_NoUnguardedGates(t *testing.T) {
	gaps, err := CheckTaskfile("../../Taskfile.yml")
	require.NoError(t, err)
	assert.Empty(t, gaps, "mache's Taskfile has unguarded gate tools: %+v", gaps)
}
