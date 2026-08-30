package smells

import (
	"fmt"
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
