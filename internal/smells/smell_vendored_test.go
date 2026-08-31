package smells

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defsIn seeds a file with n distinct definitions, so god_file's
// distinct-definition metric is a known quantity per file.
func defsIn(b *fixturedb.Builder, file string, n int) {
	for i := 0; i < n; i++ {
		id := fixturedb.ConstructID(fmt.Sprintf("%s/sym%03d", file, i))
		b.Construct(id, fixturedb.Where{Source: fixturedb.SourceID(file)})
		b.Def(fmt.Sprintf("%s_sym%03d", file, i), id, "")
	}
}

// godFileFindings runs the real god_file rule over a fixture and returns the
// flagged source_ids.
func godFileFindings(t *testing.T, seed func(*fixturedb.Builder)) []string {
	t.Helper()
	g := newSmellFixture(t, fixturedb.Standalone, seed)

	rule := RegisteredRule("god_file")
	require.NotNil(t, rule, "god_file must be registered")

	require.NoError(t, ensureSmellQueryContext(g))
	found, err := RunSmellRule(g, rule, "", 1000)
	require.NoError(t, err)

	ids := make([]string, 0, len(found))
	for _, x := range found {
		ids = append(ids, x.SourceID)
	}
	return ids
}

// TestVendoredFixtures_CannotProduceAFinding is half of mache-f41b43: 24 of
// 26 god_file findings on main were testdata/snapshots/** — vendored Rust
// that exists to be parsed. Nobody refactors it, so it is noise, and the
// baseline was ~92% vendored fixtures.
func TestVendoredFixtures_CannotProduceAFinding(t *testing.T) {
	ids := godFileFindings(t, func(b *fixturedb.Builder) {
		// A vendored file far over the god_file floor, plus ordinary files
		// to give the mean something to be.
		defsIn(b, "testdata/snapshots/vendor-corpus/huge.rs", 60)
		for i := 0; i < 8; i++ {
			defsIn(b, fmt.Sprintf("internal/pkg/small%d.go", i), 2)
		}
	})
	assert.NotContains(t, ids, "testdata/snapshots/vendor-corpus/huge.rs",
		"a vendored fixture must never be reported — it exists to be parsed, not maintained")
}

// TestVendoredFixtures_CannotMoveTheThreshold is the half that matters more,
// and the half a findings-only exclusion would miss. god_file fires at
// `n >= 10 AND n > 3 * mu`, so a vendored corpus does not merely add junk
// findings — it drags the project mean and thereby moves the bar for this
// project's own code. The rule already documents this exact mechanism from a
// different source (markdown spans pulling mu 9.79 -> 5.08, mache-50e939).
//
// The assertion is a DIFFERENTIAL: the same owned files must produce the same
// verdict whether or not a large vendored tree sits beside them.
func TestVendoredFixtures_CannotMoveTheThreshold(t *testing.T) {
	owned := func(b *fixturedb.Builder) {
		// One file just over the floor, and enough small files that mu keeps
		// it under 3*mu — i.e. NOT a finding on its own merits.
		defsIn(b, "internal/pkg/biggish.go", 12)
		for i := 0; i < 3; i++ {
			defsIn(b, fmt.Sprintf("internal/pkg/small%d.go", i), 8)
		}
	}

	without := godFileFindings(t, owned)
	with := godFileFindings(t, func(b *fixturedb.Builder) {
		owned(b)
		// A vendored corpus of many tiny files: the shape that drags mu down
		// hardest, because each contributes a small n to the average.
		for i := 0; i < 40; i++ {
			defsIn(b, fmt.Sprintf("testdata/snapshots/corpus/f%02d.rs", i), 1)
		}
	})

	assert.Equal(t, without, with,
		"adding a vendored corpus changed the verdict on this project's OWN files — "+
			"the fixtures are moving the threshold, which is the defect mache-f41b43 names")
}

// TestVendoredExclusionIsWiredIntoEveryRuleThatNeedsIt guards the set. The
// behavioural tests above cover god_file, where the subtle half (the mean)
// lives; this covers the rest by construction, because building a fixture
// that makes each rule fire on vendored input is expensive and the failure
// being guarded is trivial — someone edits a rule and drops the clause.
//
// A rule earns its place here by having been observed reporting vendored
// files on mache's own repo (mache-f41b43): 24 god_file, plus fan_out_skew,
// duplicate_definitions and long_file findings, all in
// testdata/snapshots/medium-rust-rosary/**.
func TestVendoredExclusionIsWiredIntoEveryRuleThatNeedsIt(t *testing.T) {
	for _, id := range []string{"god_file", "fan_out_skew", "duplicate_definitions", "long_file"} {
		rule := RegisteredRule(id)
		require.NotNilf(t, rule, "%s must be registered", id)
		assert.Containsf(t, rule.Query, "v_vendored_files",
			"%s reported vendored fixtures before this exclusion existed; dropping it "+
				"puts third-party code nobody owns back into the baseline", id)
	}

	// god_file and fan_out_skew are the mean-relative pair: they must exclude
	// BEFORE computing mu, or a vendored tree moves the bar for this
	// project's own code even while its own findings are hidden.
	for _, id := range []string{"god_file", "fan_out_skew"} {
		rule := RegisteredRule(id)
		q := rule.Query
		firstExclusion := strings.Index(q, "v_vendored_files")
		avg := strings.Index(strings.ToUpper(q), "AVG(")
		require.Positivef(t, avg, "%s is expected to compute a mean", id)
		assert.Lessf(t, firstExclusion, avg,
			"%s excludes vendored files only AFTER computing the mean — the findings would be "+
				"clean while the threshold stayed distorted, which is the half-fix this bead rejects", id)
	}
}
