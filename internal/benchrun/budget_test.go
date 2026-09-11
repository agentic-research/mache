package benchrun

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBudget() Budget {
	return Budget{
		PeakRSSBytes:      2_000_000_000,
		LeylineDBBytes:    500_000_000,
		ProjectionDBBytes: 100_000_000,
		WallMsAdvisory:    30_000,
	}
}

// TestBudget_Check_ReportsEveryHardLimit: a run over on two axes must report
// both. Stopping at the first would let a fix for one hide the other, and the
// cold-path decomposition is the whole reason the numbers are separate.
func TestBudget_Check_ReportsEveryHardLimit(t *testing.T) {
	v := testBudget().Check(Observed{
		PeakRSSBytes:      3_500_000_000,
		LeylineDBBytes:    1_900_000_000,
		ProjectionDBBytes: 60_000_000,
	})
	require.Len(t, v, 2)
	assert.Equal(t, "peak RSS", v[0].Metric)
	assert.Equal(t, "leyline db", v[1].Metric)
	assert.Contains(t, v[0].String(), "3.50 GB")
	assert.Contains(t, v[0].String(), "2.00 GB")
}

// TestBudget_Check_WithinBudgetIsSilent, including exactly at the limit: the
// budget is a ceiling, not an exclusive bound.
func TestBudget_Check_WithinBudgetIsSilent(t *testing.T) {
	assert.Empty(t, testBudget().Check(Observed{
		PeakRSSBytes:      2_000_000_000,
		LeylineDBBytes:    500_000_000,
		ProjectionDBBytes: 100_000_000,
	}))
}

// TestBudget_Check_WallNeverFails is a design decision, pinned: wall time is
// advisory. Shared CI runners vary by more than any honest tolerance band, a
// timing gate that flakes gets disabled, and a disabled gate is worse than an
// advisory number. A run ten times over the wall target and inside every byte
// limit is a PASS.
func TestBudget_Check_WallNeverFails(t *testing.T) {
	assert.Empty(t, testBudget().Check(Observed{
		PeakRSSBytes:      1_000_000_000,
		LeylineDBBytes:    100_000_000,
		ProjectionDBBytes: 10_000_000,
		Wall:              300 * time.Second,
	}))
}

// TestLoadBudget_RejectsAnUnlimitedLimit is the anti-weasel test. A missing or
// zero limit read as "no ceiling" turns one typo into a permanently green
// gate — the single failure mode a budget file must not have.
func TestLoadBudget_RejectsAnUnlimitedLimit(t *testing.T) {
	for name, body := range map[string]string{
		"missing peak_rss": "[budget]\nleyline_db_bytes = 1\nprojection_db_bytes = 1\n",
		"zero leyline_db":  "[budget]\npeak_rss_bytes = 1\nleyline_db_bytes = 0\nprojection_db_bytes = 1\n",
		"negative projection": "[budget]\npeak_rss_bytes = 1\nleyline_db_bytes = 1\n" +
			"projection_db_bytes = -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "b.toml")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			_, err := LoadBudget(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be positive")
		})
	}

	_, err := LoadBudget(filepath.Join(t.TempDir(), "absent.toml"))
	assert.Error(t, err, "a budget file that is not there must not read as an empty budget")
}

// TestLoadBudget_CommittedFileIsTheEnvelope reads the real file: the committed
// numbers ARE the claim (mache-2de6c0's 2.0 GB / 0.5 GB / 0.1 GB envelope), so
// a silent edit to any of them should fail here and be argued about in review.
func TestLoadBudget_CommittedFileIsTheEnvelope(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	b, err := LoadBudget(filepath.Join(root, "testdata", "snapshots", "cold-budget.toml"))
	require.NoError(t, err)

	assert.Equal(t, int64(2_000_000_000), b.PeakRSSBytes, "the 4-core/16 GB envelope is 2.0 GB peak RSS")
	assert.Equal(t, int64(500_000_000), b.LeylineDBBytes)
	assert.Equal(t, int64(100_000_000), b.ProjectionDBBytes)
	assert.Equal(t, 30_000, b.WallMsAdvisory)
}
