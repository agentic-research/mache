package ingest

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/internal/sqlcount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Growth-class gate for the projection's call extraction (mache-c0537f).
//
// mache shipped an O(nodes²) projection regression through green CI
// (mache-4f3840). Its correctness fix was proven afterwards by a hand-built
// byte diff between two binaries, not by a test, and nothing has stood between
// that path and a repeat since.
//
// What is asserted is a COUNT of SQL statements, not a duration. That choice is
// the whole point:
//
//   - Wall-clock cannot separate "slower runner" from "wrong complexity class",
//     and this repo has the receipts on timing gates — testdata/snapshots/
//     baselines.toml pins a wall_ms that needs manual bumps, and mache-434ecc
//     is an open flake where a 50ms ticker missed an 80ms window on a loaded
//     CI runner.
//   - The regression's actual signature is a query explosion. The old code ran
//     a per-scope query loop; ExtractCalls now issues one query per registered
//     pattern. That is a CONSTANT, and a constant is a far sharper thing to
//     assert than a number of milliseconds.
//
// docs/reference/projection-performance.md records the fix as n^2.02 -> n^0.92
// and warns against quoting any single "N× faster" figure, since the ratio is
// itself proportional to n. A gate that pinned one wall-time would be that same
// mistake in gate form.
//
// Note the sweep is per-FILE call count, not repo size. The quadratic was
// per-file: a repo of many small files barely exercises it, which is why this
// is a synthetic sweep rather than a large external corpus.

// extractCallsStatements counts the SQL statements one ExtractCalls run issues
// against a file seeded with nCalls calls.
func extractCallsStatements(t *testing.T, nCalls int) int64 {
	t.Helper()

	// Seed through the ordinary driver: seeding work is not under measurement.
	seeded := seedManyCalls(t, nCalls)
	require.NoError(t, seeded.DB().Close())

	// Re-open the same file through the counting driver, so the only statements
	// recorded are the ones ExtractCalls itself issues.
	sqlcount.RegisterDriver()
	db, err := sql.Open(sqlcount.DriverName, seeded.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	w := NewASTWalker(db)
	// Warm once on a DIFFERENT file: the first run may prepare statements or
	// populate per-walker state, and that one-time cost is not part of the
	// growth question — but warming on main.go itself would cache its section
	// and leave nothing to count (the cold cost of the measured file is the
	// question).
	_, err = w.ExtractCalls("other.go", "go")
	require.NoError(t, err)

	count := sqlcount.Reset()
	calls, err := w.ExtractCalls("main.go", "go")
	require.NoError(t, err)
	require.NotEmpty(t, calls, "fixture produced no calls — the gate would be vacuous")
	return count()
}

// TestExtractCalls_StatementCountDoesNotGrowWithFileSize is the gate.
//
// ExtractCalls issues one query per registered call pattern, so the count is a
// property of the LANGUAGE, not of the file. A file with fifty times the calls
// must cost the same number of statements; anything else means a per-node or
// per-scope loop came back.
func TestExtractCalls_StatementCountDoesNotGrowWithFileSize(t *testing.T) {
	const small, large = 10, 500

	atSmall := extractCallsStatements(t, small)
	atLarge := extractCallsStatements(t, large)

	require.Positive(t, atSmall, "no statements counted — the instrument is not wired to the walker")
	assert.Equal(t, atSmall, atLarge, fmt.Sprintf(
		"ExtractCalls issued %d statements for %d calls but %d for %d.\n\n"+
			"The count must be independent of how many calls a file contains — it is the\n"+
			"one statement that loads the file's node section (every registered pattern\n"+
			"is then evaluated in memory). Growth here means a per-scope or per-node\n"+
			"query loop is back, which is the shape of the O(nodes²) projection regression\n"+
			"(mache-4f3840) that shipped through green CI once already.",
		atSmall, small, atLarge, large))
}

// TestExtractCalls_ResultIsUnchangedAcrossSizes keeps the gate honest.
//
// A statement count is easy to hold flat by extracting nothing. This pins that
// the work still happens: the call count scales with the file, even though the
// statement count does not.
func TestExtractCalls_ResultIsUnchangedAcrossSizes(t *testing.T) {
	countCalls := func(n int) int {
		calls, err := NewASTWalker(seedManyCalls(t, n).DB()).ExtractCalls("main.go", "go")
		require.NoError(t, err)
		return len(calls)
	}

	small, large := countCalls(10), countCalls(500)
	assert.Greater(t, large, small,
		"a bigger file must yield more calls — otherwise a flat statement count proves nothing")
	assert.GreaterOrEqual(t, large, 400,
		"the large fixture should extract on the order of its seeded call count")
}
