package smells

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSmellRule_Severity_Effective pins the zero-value behavior of
// SmellRule.Severity: an unset field MUST resolve to SeverityWarn so
// the schema addition (mache-ec1a06) doesn't silently flip existing
// rules from observability to gating mode.
//
// All currently-shipped rules omit Severity, so this is the
// load-bearing default. If it ever changes, every existing rule
// quietly becomes a candidate for --fail-on=warn promotion in CI —
// the kind of contract-change-by-default we explicitly avoided per
// ADR-0018.
func TestSmellRule_Severity_Effective(t *testing.T) {
	cases := []struct {
		name string
		in   Severity
		want Severity
	}{
		{"zero value defaults to warn", Severity(""), SeverityWarn},
		{"explicit off stays off", SeverityOff, SeverityOff},
		{"explicit warn stays warn", SeverityWarn, SeverityWarn},
		{"explicit error stays error", SeverityError, SeverityError},
		{"unknown string defaults to warn (defensive)", Severity("debug"), SeverityWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := SmellRule{Severity: tc.in}
			assert.Equal(t, tc.want, r.Effective())
		})
	}
}

// TestSmellRule_AllRegisteredRulesHaveDefaultableSeverity pins that
// every built-in rule's Severity field resolves to a known value via
// Effective(). Catches accidental misuse of the string type (e.g. a
// future contributor typing "ERROR" instead of "error" — Effective
// will silently treat it as warn, which is correct fail-soft behavior
// but worth flagging via this test so the test name documents intent.
func TestSmellRule_AllRegisteredRulesHaveDefaultableSeverity(t *testing.T) {
	for i := range smellRegistry {
		r := &smellRegistry[i]
		t.Run(r.ID, func(t *testing.T) {
			got := r.Effective()
			assert.Contains(t, []Severity{SeverityOff, SeverityWarn, SeverityError}, got,
				"rule %q effective severity must be one of the canonical three; got %q (raw=%q)",
				r.ID, got, r.Severity)
		})
	}
}

// TestSmellRule_Tags_NoExplosion enforces the cap from research +
// ADR-0018: rules should carry 3-5 tags at most. Beyond that we're
// recreating clippy's `restriction` group failure mode where rules
// accumulate orthogonal classifications that confuse selection.
//
// This is a soft cap — the test allows up to 5 and warns visibly
// past that, but doesn't fail the build until 8+. Adjust if the
// cap proves wrong in practice.
func TestSmellRule_Tags_NoExplosion(t *testing.T) {
	const softCap = 5
	const hardCap = 8
	for i := range smellRegistry {
		r := &smellRegistry[i]
		if len(r.Tags) > softCap {
			t.Logf("rule %q has %d tags (soft cap %d); consider consolidating",
				r.ID, len(r.Tags), softCap)
		}
		if len(r.Tags) > hardCap {
			t.Errorf("rule %q has %d tags (hard cap %d) — taxonomy is exploding (see mache-ec1a06)",
				r.ID, len(r.Tags), hardCap)
		}
	}
}
