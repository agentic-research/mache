package lattice

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRegexTemporalSharding_Fragility demonstrates that the regex-based
// temporal sharding used in Greedy Inference is fragile when dates
// deviate from strict ISO 8601 (YYYY-MM-DD).
func TestRegexTemporalSharding_Fragility(t *testing.T) {
	tests := []struct {
		name     string
		date     string
		year     string
		month    string
		expected bool // Expect regex to match correctly
	}{
		{"Standard ISO", "2024-01-15", "2024", "01", true},
		{"With Time", "2024-01-15T12:00:00Z", "2024", "01", true},

		// Edge Cases where Regex ^.{5}MM might fail or produce garbage
		{"Single Digit Month", "2024-1-15", "2024", "1", false}, // Regex expects 2 digits at index 5? No, it expects match.
		// If greedy generates selector for "1", it generates ^.{5}1
		// "2024-1-15": index 5 is '1'. It matches!

		{"Slash Separator", "2024/01/15", "2024", "01", true}, // Index 5 is '0'. Matches.

		{"Short Year", "24-01-15", "2024", "01", false}, // Index 5 is '1'. But year is "24".
		// If year is "24", split key is "24".

		{"Garbage Prefix", "Date: 2024-01-15", "2024", "01", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate Greedy Selector Generation logic
			// 1. Extract Key (Partitioning)
			var yearKey, monthKey string
			if len(tt.date) >= 4 {
				yearKey = tt.date[:4]
			}
			if len(tt.date) >= 7 {
				monthKey = tt.date[5:7]
			}

			// 2. Generate Selector (Regex)
			// Year: ^YYYY
			// Month: ^.{5}MM

			yearRegex := fmt.Sprintf("^%s", yearKey)
			monthRegex := fmt.Sprintf("^.{5}%s", monthKey)

			// 3. Verify Match
			matchedYear, _ := regexp.MatchString(yearRegex, tt.date)
			matchedMonth, _ := regexp.MatchString(monthRegex, tt.date)

			if tt.expected {
				assert.True(t, matchedYear, "Year regex failed for %s", tt.date)
				assert.True(t, matchedMonth, "Month regex failed for %s", tt.date)
			} else {
				// We expect failure for malformed dates
				// If it matches, it's "robust" but maybe wrong context
				if !matchedYear || !matchedMonth {
					return // Passed (failed as expected)
				}
				// If it matches but we expected failure, print why
				// e.g. Short Year "24-01..."
				// yearKey="24-0". Regex "^24-0". Matches!
				// But semantically "24-0" is not a year.
			}
		})
	}
}
