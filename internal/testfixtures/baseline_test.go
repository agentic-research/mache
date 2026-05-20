package testfixtures

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingFataler is a *testing.T-compatible stub (via the fataler
// interface) that captures Fatalf invocations without runtime.Goexit-
// ing the test goroutine. The production AssertWithinBaseline takes
// *testing.T; the helper-level assertWithinBaselineFromMap takes a
// fataler so tests can observe both pass and fail paths.
type recordingFataler struct {
	failed bool
	msg    string
	logs   []string
}

func (r *recordingFataler) Helper() {}

func (r *recordingFataler) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

func (r *recordingFataler) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// fakeBaselines builds a map of baselines without touching disk. Used
// by the helper-level tests below so they don't depend on
// testdata/snapshots/baselines.toml's actual state.
func fakeBaselines() map[string][]Baseline {
	return map[string][]Baseline{
		"find_smells:dead_code": {
			{
				Fixture:        "mache-self",
				WallMs:         100,
				TolerancePct:   25,
				AnchoredAt:     "deadbee",
				AnchoredAtDate: "2026-05-19",
			},
		},
	}
}

// TestAssertWithinBaseline_InRange_Passes proves that a measurement
// equal to the baseline (well within the 25% tolerance) does not
// fire t.Fatalf.
func TestAssertWithinBaseline_InRange_Passes(t *testing.T) {
	rec := &recordingFataler{}
	assertWithinBaselineFromMap(rec, fakeBaselines(),
		"find_smells:dead_code", "mache-self", 100*time.Millisecond)
	assert.False(t, rec.failed, "in-range measurement must not fail the gate (msg=%q)", rec.msg)
	assert.NotEmpty(t, rec.logs, "passing path should still emit a log line so reviewers can see headroom")
}

// TestAssertWithinBaseline_AtTolerance_Passes proves that a measurement
// equal to baseline * (1 + tolerance) exactly is treated as passing.
// The gate uses strict "exceeds" semantics ("> ceiling", not ">="):
// flipping the comparator would shift gate behavior at the boundary
// the ADR-0019 D.6 derivation assumes.
func TestAssertWithinBaseline_AtTolerance_Passes(t *testing.T) {
	rec := &recordingFataler{}
	// 100ms * 1.25 = 125ms exactly.
	assertWithinBaselineFromMap(rec, fakeBaselines(),
		"find_smells:dead_code", "mache-self", 125*time.Millisecond)
	assert.False(t, rec.failed, "measurement at exact tolerance boundary must pass (msg=%q)", rec.msg)
}

// TestAssertWithinBaseline_BeyondTolerance_Fails proves that a
// measurement past the tolerance ceiling fires t.Fatalf with a
// rebaseline command in the error message. Both numbers (measured +
// ceiling) must be present so the developer can see how far over they
// are; the rebaseline command must be present so the path forward is
// discoverable without reading the helper source.
func TestAssertWithinBaseline_BeyondTolerance_Fails(t *testing.T) {
	rec := &recordingFataler{}
	// 100ms * 1.30 = 130ms; ceiling is 125ms. Must fail.
	assertWithinBaselineFromMap(rec, fakeBaselines(),
		"find_smells:dead_code", "mache-self", 130*time.Millisecond)

	require.True(t, rec.failed, "measurement beyond tolerance must fail the gate")
	assert.Contains(t, rec.msg, "exceeds ceiling",
		"failure message must explain the band was exceeded")
	assert.Contains(t, rec.msg, "task fixtures:rebaseline",
		"failure message must surface the rebaseline command for the developer")
	// Sanity: both numbers should appear so the developer can size the regression.
	assert.Contains(t, rec.msg, "130", "measured ms should appear in the failure message")
	assert.Contains(t, rec.msg, "125", "ceiling ms should appear in the failure message")
}

// TestAssertWithinBaseline_MissingBaseline_FailsWithGuidance proves
// that querying an unknown (key, fixture) pair fires t.Fatalf with a
// guidance message pointing at the rebaseline command. The gate
// refuses to silently pass when no baseline anchor exists — that
// would let new perf-bearing rules ship without a recorded budget.
func TestAssertWithinBaseline_MissingBaseline_FailsWithGuidance(t *testing.T) {
	rec := &recordingFataler{}
	assertWithinBaselineFromMap(rec, fakeBaselines(),
		"unknown:rule", "mache-self", 50*time.Millisecond)

	require.True(t, rec.failed, "unknown key must fail the gate, not silently pass")
	assert.Contains(t, rec.msg, "no baseline recorded",
		"failure message must explain the absence so devs can distinguish from a real regression")
	assert.Contains(t, rec.msg, "task fixtures:rebaseline",
		"failure message must tell the developer how to set the initial anchor")
}

// TestAssertWithinBaseline_UnknownFixtureForKnownKey_FailsWithGuidance
// proves that a (known key, unknown fixture) pair also fires the
// missing-baseline path. This matters because real perf rules may be
// scoped to specific fixtures (e.g. find_smells:dead_code anchored on
// mache-self only); other fixtures shouldn't accidentally pass via a
// neighbor's anchor.
func TestAssertWithinBaseline_UnknownFixtureForKnownKey_FailsWithGuidance(t *testing.T) {
	rec := &recordingFataler{}
	assertWithinBaselineFromMap(rec, fakeBaselines(),
		"find_smells:dead_code", "medium-rust-rosary", 50*time.Millisecond)

	require.True(t, rec.failed, "known key + unknown fixture must fail")
	assert.Contains(t, rec.msg, "no baseline recorded",
		"failure message must explain the absence")
}

// TestParseBaselinesFile_RoundTrips proves the parser accepts the
// on-disk format the rebaseline tool emits.
func TestParseBaselinesFile_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baselines.toml")
	content := `schema = "mache-baselines/v1"

[["find_smells:dead_code"]]
fixture = "mache-self"
wall_ms = 124
tolerance_pct = 25
anchored_at = "abc1234"
anchored_at_date = "2026-05-19"
last_intentional_bump = ""
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := parseBaselinesFile(path)
	require.NoError(t, err)

	b, ok := LookupBaseline(parsed, "find_smells:dead_code", "mache-self")
	require.True(t, ok, "round-tripped baseline should be lookable by (key, fixture)")
	assert.Equal(t, 124, b.WallMs)
	assert.Equal(t, 25, b.TolerancePct)
	assert.Equal(t, "abc1234", b.AnchoredAt)
	assert.Equal(t, "2026-05-19", b.AnchoredAtDate)
}

// TestParseBaselinesFile_RejectsWrongSchema proves the parser refuses
// a stale or future schema string. This is the load-bearing forward-
// compat hatch: a future format change can be flagged at parse time
// rather than silently misinterpreting fields.
func TestParseBaselinesFile_RejectsWrongSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baselines.toml")
	require.NoError(t, os.WriteFile(path, []byte(`schema = "mache-baselines/v999"
`), 0o644))

	_, err := parseBaselinesFile(path)
	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "unsupported baselines schema"),
		"error must call out the schema mismatch, got %q", err.Error())
}
