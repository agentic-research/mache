package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewriteEntry_UpdatesWallMsAndPreservesComments proves the
// surgical text edit preserves hand-authored comments and inline
// trailing comments — the reason we use line-oriented rewriting
// instead of round-tripping via the TOML encoder.
func TestRewriteEntry_UpdatesWallMsAndPreservesComments(t *testing.T) {
	in := `schema = "mache-baselines/v1"

# Perf-gate baselines (ADR-0019 D.6).
# Fixed-anchor + tolerance band. Auto-rebaseline is intentionally NOT
# done — see ADR-0019 D.6.

[["find_smells:dead_code"]]
fixture = "mache-self"
wall_ms = 124              # FIXED at ADR-0019 PR 3 acceptance
tolerance_pct = 25         # gate fails if measurement > wall_ms * 1.25
anchored_at = "f1c6d9a"    # mache HEAD at baseline-set time
anchored_at_date = "2026-05-19"
last_intentional_bump = ""
`
	out, err := rewriteEntry(in, "find_smells:dead_code", "mache-self",
		200, "abcdef0", "2026-05-20", "2026-05-20: feature X regression")
	require.NoError(t, err)

	assert.Contains(t, out, "wall_ms = 200",
		"wall_ms must be updated")
	assert.Contains(t, out, `anchored_at = "abcdef0"`,
		"anchored_at must be updated to new SHA")
	assert.Contains(t, out, `anchored_at_date = "2026-05-20"`,
		"anchored_at_date must be updated to today")
	assert.Contains(t, out, `last_intentional_bump = "2026-05-20: feature X regression"`,
		"last_intentional_bump must carry the audit-trail justification")

	// Comments must survive — both the file header AND inline
	// trailing comments on the rewritten lines. If they vanish the
	// audit trail (which lives in the comments) is destroyed.
	assert.Contains(t, out, "# Perf-gate baselines (ADR-0019 D.6).",
		"top-of-file comments must survive")
	assert.Contains(t, out, "# FIXED at ADR-0019 PR 3 acceptance",
		"inline trailing comment on wall_ms must survive the rewrite")
	assert.Contains(t, out, "# gate fails if measurement > wall_ms * 1.25",
		"inline trailing comment on tolerance_pct must survive")
	assert.Contains(t, out, "# mache HEAD at baseline-set time",
		"inline trailing comment on anchored_at must survive")
}

// TestRewriteEntry_MultipleEntriesForSameKey proves the rewriter
// picks the right entry when one key has multiple fixtures. This is
// the future-proofing for when a rule gets baselined on more than
// one corpus (e.g. mache-self + medium-rust-rosary).
func TestRewriteEntry_MultipleEntriesForSameKey(t *testing.T) {
	in := `schema = "mache-baselines/v1"

[["find_smells:dead_code"]]
fixture = "mache-self"
wall_ms = 124
tolerance_pct = 25
anchored_at = "f1c6d9a"
anchored_at_date = "2026-05-19"
last_intentional_bump = ""

[["find_smells:dead_code"]]
fixture = "medium-rust-rosary"
wall_ms = 333
tolerance_pct = 25
anchored_at = "f1c6d9a"
anchored_at_date = "2026-05-19"
last_intentional_bump = ""
`
	out, err := rewriteEntry(in, "find_smells:dead_code", "medium-rust-rosary",
		444, "newSHA0", "2026-05-20", "2026-05-20: bumped rust fixture")
	require.NoError(t, err)

	// mache-self entry must be UNCHANGED.
	assert.Contains(t, out, "wall_ms = 124",
		"unrelated entry's wall_ms must not be touched")
	// rust-rosary entry must be updated.
	assert.Contains(t, out, "wall_ms = 444",
		"target entry's wall_ms must be updated")
	assert.Contains(t, out, `anchored_at = "newSHA0"`,
		"target entry's anchored_at must be updated")
}

// TestRewriteEntry_UnknownFixtureReturnsError proves the rewriter
// refuses to silently leave the file unchanged when given a fixture
// that doesn't appear in any block for the key. Defense-in-depth on
// top of the ParseBaselinesFile + LookupBaseline check in run().
func TestRewriteEntry_UnknownFixtureReturnsError(t *testing.T) {
	in := `schema = "mache-baselines/v1"

[["find_smells:dead_code"]]
fixture = "mache-self"
wall_ms = 124
tolerance_pct = 25
anchored_at = "f1c6d9a"
anchored_at_date = "2026-05-19"
last_intentional_bump = ""
`
	_, err := rewriteEntry(in, "find_smells:dead_code", "ghost-fixture",
		200, "abcdef0", "2026-05-20", "n/a")
	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "ghost-fixture") ||
			strings.Contains(err.Error(), "no entry has fixture"),
		"error must name the missing fixture or the lookup failure, got: %v", err)
}

// TestRewriteEntry_UnknownKeyReturnsError proves the rewriter refuses
// to fabricate a [[key]] block. Adding new gates requires deliberate
// manual edits to baselines.toml (so reviewers see the new anchor in
// the diff).
func TestRewriteEntry_UnknownKeyReturnsError(t *testing.T) {
	in := `schema = "mache-baselines/v1"

[["find_smells:dead_code"]]
fixture = "mache-self"
wall_ms = 124
tolerance_pct = 25
anchored_at = "f1c6d9a"
anchored_at_date = "2026-05-19"
last_intentional_bump = ""
`
	_, err := rewriteEntry(in, "find_smells:new_rule", "mache-self",
		200, "abcdef0", "2026-05-20", "n/a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no [[find_smells:new_rule]]",
		"error must name the missing key, got: %v", err)
}

// TestEscapeTOML proves the justification escaper handles the only
// metacharacters in a TOML basic string (\" and \\).
func TestEscapeTOML(t *testing.T) {
	assert.Equal(t, `plain text`, escapeTOML(`plain text`))
	assert.Equal(t, `with \"quotes\"`, escapeTOML(`with "quotes"`))
	assert.Equal(t, `back\\slash`, escapeTOML(`back\slash`))
	assert.Equal(t, `both \\ and \"`, escapeTOML(`both \ and "`))
}
