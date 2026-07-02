package leyline

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeylineSchemaBinaryVersionParity is the build-time drift guard for
// bead mache-b8af69. mache carries two independent leyline versions:
//
//   - the go.mod pin of the Go schema client
//     (github.com/agentic-research/ley-line-open/clients/go/leyline-schema)
//   - the leylineBinaryVersion const (the daemon binary mache downloads)
//
// Per socket.go's design note, the *patch* may float (a daemon-only LSP fix
// ships a binary with no wire change), but the MAJOR.MINOR must match because
// that is the wire/schema format the Go client decodes. Today only a comment
// and a human keeping two pins in lockstep enforce that — the exact setup
// that produced the stale-cached-0.4.1 skew (mache-0acdf6). This test makes
// the invariant executable: a minor/major bump to one without the other fails
// CI. The dynamic runtime handshake is the complementary mache-8kif (blocked
// on LLO shipping a daemon `version` op).
func TestLeylineSchemaBinaryVersionParity(t *testing.T) {
	root := repoRoot(t)
	modBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)

	// The require line: "<module> vMAJOR.MINOR.PATCH[...]". Pseudo-versions
	// still lead with vMAJOR.MINOR.PATCH, so the base captures fine.
	schemaRE := regexp.MustCompile(`(?m)^\s*github\.com/agentic-research/ley-line-open/clients/go/leyline-schema\s+v(\d+)\.(\d+)\.\d+`)
	sm := schemaRE.FindStringSubmatch(string(modBytes))
	require.NotNil(t, sm, "leyline-schema require line not found in go.mod")
	schemaMM := sm[1] + "." + sm[2]

	binRE := regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)
	bm := binRE.FindStringSubmatch(leylineBinaryVersion)
	require.NotNil(t, bm, "leylineBinaryVersion %q is not a vMAJOR.MINOR.PATCH tag", leylineBinaryVersion)
	binMM := bm[1] + "." + bm[2]

	assert.Equal(t, schemaMM, binMM,
		"leyline-schema go.mod pin (v%s.x) and leylineBinaryVersion const (%s) must agree on major.minor — "+
			"that's the wire format. Patch may float; a minor/major bump must update BOTH (mache-b8af69; see socket.go's leylineBinaryVersion note).",
		schemaMM, leylineBinaryVersion)
}

// repoRoot walks up from the test's working directory to the module root
// (the directory containing go.mod).
func repoRoot(t *testing.T) string {
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
