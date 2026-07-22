package lattice

import "testing"

// TestLooksLikeDatePrefix characterizes the ISO-date detection used to set
// FieldStats.IsDate (which drives temporal sharding in greedy.go). It replaced
// a `^\d{4}-\d{2}` regex with a structural digit check; these cases pin the
// exact semantics so the two are provably equivalent — a PREFIX match (trailing
// content allowed), no month-range validation (13 is "a date" to this
// heuristic, exactly as the regex treated it).
func TestLooksLikeDatePrefix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		{"2024-01", true, "bare YYYY-MM"},
		{"2024-01-15", true, "full ISO date — prefix match"},
		{"2024-01-15T10:30:00Z", true, "RFC3339 timestamp — prefix match"},
		{"1999-12 something", true, "trailing text allowed (prefix, not anchored at end)"},
		{"2024-13", true, "no month-range validation — matches the old regex exactly"},
		{"2024-00", true, "same: digits only, no range check"},

		{"2024", false, "too short"},
		{"2024-0", false, "too short (6 chars)"},
		{"202-01", false, "only 3 leading digits"},
		{"20244-01", false, "5th char must be '-'"},
		{"2024/01", false, "separator must be '-'"},
		{"2024-ab", false, "month chars must be digits"},
		{"abcd-01", false, "year chars must be digits"},
		{" 2024-01", false, "not anchored at start — leading space fails"},
		{"", false, "empty"},
		{"v2024-01", false, "prefixed, not anchored"},
	} {
		if got := looksLikeDatePrefix(tc.in); got != tc.want {
			t.Errorf("looksLikeDatePrefix(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}
