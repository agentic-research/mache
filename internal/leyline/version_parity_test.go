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
// The invariant (mache-b8af69): the go.mod leyline-schema client must decode
// the wire the pinned binary speaks. The binary and the schema Go module are
// tagged on DIFFERENT cadences — the binary bumps every release, the schema
// module (clients/go/leyline-schema) only when the capnp wire types change —
// so equal-minor is NOT the contract (it held only by coincidence while
// everything was 0.7.x, and v0.8.0 broke that: binary v0.8.0, newest schema
// tag v0.7.1, wire unchanged). The real contract is a RANGE: the schema pin
// must sit in [leylineSchemaCompatFloor, binary]. Below the floor the wire is
// incompatible; above the binary the client expects types the daemon doesn't
// emit. This makes the invariant executable — a schema/binary pair that falls
// outside the range fails CI. Complementary runtime handshake: mache-8kif.
func TestLeylineSchemaBinaryVersionParity(t *testing.T) {
	root := repoRoot(t)
	modBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)

	// The require line: "<module> vMAJOR.MINOR.PATCH[...]". Pseudo-versions
	// still lead with vMAJOR.MINOR.PATCH, so the base captures fine.
	schemaRE := regexp.MustCompile(`(?m)^\s*github\.com/agentic-research/ley-line-open/clients/go/leyline-schema\s+(v\d+\.\d+\.\d+)`)
	sm := schemaRE.FindStringSubmatch(string(modBytes))
	require.NotNil(t, sm, "leyline-schema require line not found in go.mod")
	schemaVer := sm[1]

	// schema pin must be >= the binary's wire compat floor …
	assert.GreaterOrEqual(t, compareSemver(schemaVer, leylineSchemaCompatFloor), 0,
		"leyline-schema go.mod pin (%s) is below the binary's wire compat floor (%s) — "+
			"the client can't decode %s's wire (mache-b8af69; bump the schema pin or the floor)",
		schemaVer, leylineSchemaCompatFloor, leylineBinaryVersion)

	// … and <= the binary version (the client must not expect newer types
	// than the daemon emits).
	assert.LessOrEqual(t, compareSemver(schemaVer, leylineBinaryVersion), 0,
		"leyline-schema go.mod pin (%s) is newer than the pinned binary (%s) — "+
			"the client expects wire types the daemon may not emit (mache-b8af69)",
		schemaVer, leylineBinaryVersion)
}

// schemaInBinaryRange is the pure predicate the parity gate asserts: a schema
// client version is compatible iff floor <= schema <= binary. Extracted so the
// boundary semantics are pinned with synthetic values, independent of the live
// go.mod pin and consts.
func schemaInBinaryRange(schema, floor, binary string) bool {
	return compareSemver(schema, floor) >= 0 && compareSemver(schema, binary) <= 0
}

func TestSchemaInBinaryRange_Boundaries(t *testing.T) {
	const floor, binary = "v0.6.0", "v0.8.0"
	cases := []struct {
		schema string
		want   bool
		why    string
	}{
		{"v0.7.1", true, "the real v0.8.0 case: wire unchanged, schema lags the binary minor"},
		{"v0.6.0", true, "exactly at the floor is compatible"},
		{"v0.8.0", true, "exactly at the binary is compatible"},
		{"v0.5.9", false, "below the floor: wire incompatible"},
		{"v0.8.1", false, "above the binary: client expects types the daemon may not emit"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, schemaInBinaryRange(c.schema, floor, binary),
			"schema=%s floor=%s binary=%s — %s", c.schema, floor, binary, c.why)
	}
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
