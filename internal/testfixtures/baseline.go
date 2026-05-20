// Baseline tracking for perf gates (ADR-0019 PR 3).
//
// AssertWithinBaseline reads testdata/snapshots/baselines.toml and
// fails the test if the measured wall-time exceeds the recorded
// baseline by more than tolerance_pct percent.
//
// IMPORTANT: this helper does NOT auto-update the baseline on success.
// Per ADR-0019 D.6, the perf gate is fixed-anchor — explicit rebaseline
// via `task fixtures:rebaseline` is the only way to bump wall_ms.
// Auto-rebaseline would convert the gate into a perf ratchet that only
// loosens (math: at 25% tolerance, 4 merges of +20% compound to 2.07x
// slower without the gate firing — see ADR-0019 D.6 for the derivation).
package testfixtures

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// Baseline is one entry in baselines.toml — the per-(key, fixture)
// fixed anchor + tolerance band that perf gates assert against.
//
// `Key` is implicit: it's the table-array name in TOML (e.g. the key
// in [["find_smells:dead_code"]] is "find_smells:dead_code").
// Multiple baselines per key allow one rule to have entries for
// several fixtures.
type Baseline struct {
	Fixture             string `toml:"fixture"`
	WallMs              int    `toml:"wall_ms"`
	TolerancePct        int    `toml:"tolerance_pct"`
	AnchoredAt          string `toml:"anchored_at"`
	AnchoredAtDate      string `toml:"anchored_at_date"`
	LastIntentionalBump string `toml:"last_intentional_bump"`
}

// baselinesPath returns the on-disk path to baselines.toml under
// mache's repo root. Exposed as a var so tests can override.
var baselinesPath = func() (string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "testdata", "snapshots", "baselines.toml"), nil
}

var (
	baselinesOnce sync.Once
	baselinesVal  map[string][]Baseline
	baselinesErr  error
)

// loadBaselines parses baselines.toml. Cached per-process.
//
// The TOML structure uses arbitrary table-array names as keys (e.g.
// [["find_smells:dead_code"]]). We parse into a generic map so the
// helper doesn't have to enumerate every rule at compile time.
func loadBaselines() (map[string][]Baseline, error) {
	baselinesOnce.Do(func() {
		path, err := baselinesPath()
		if err != nil {
			baselinesErr = err
			return
		}
		baselinesVal, baselinesErr = ParseBaselinesFile(path)
	})
	return baselinesVal, baselinesErr
}

// ParseBaselinesFile parses one baselines.toml at the given path.
// Extracted (and exported) so tooling (tools/fixtures-rebaseline) and
// tests can exercise the parser against arbitrary files without
// touching the package-level cache.
func ParseBaselinesFile(path string) (map[string][]Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baselines %s: %w", path, err)
	}
	// Parse into a generic map first to capture the table-array names
	// (which TOML's struct binding can't reach because they're
	// user-defined keys, not fixed field names).
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse baselines %s: %w", path, err)
	}
	schema, _ := raw["schema"].(string)
	if schema != "mache-baselines/v1" {
		return nil, fmt.Errorf("unsupported baselines schema %q (want mache-baselines/v1)", schema)
	}
	out := make(map[string][]Baseline)
	for k, v := range raw {
		if k == "schema" {
			continue
		}
		entries, ok := v.([]map[string]any)
		if !ok {
			// Single-table syntax instead of array-of-tables: accept
			// as a 1-element list for forgiveness.
			if single, ok2 := v.(map[string]any); ok2 {
				entries = []map[string]any{single}
			} else {
				return nil, fmt.Errorf("baseline %q: expected array of tables, got %T", k, v)
			}
		}
		baselines := make([]Baseline, 0, len(entries))
		for _, e := range entries {
			b := Baseline{}
			if s, ok := e["fixture"].(string); ok {
				b.Fixture = s
			}
			if n, ok := e["wall_ms"].(int64); ok {
				b.WallMs = int(n)
			}
			if n, ok := e["tolerance_pct"].(int64); ok {
				b.TolerancePct = int(n)
			}
			if s, ok := e["anchored_at"].(string); ok {
				b.AnchoredAt = s
			}
			if s, ok := e["anchored_at_date"].(string); ok {
				b.AnchoredAtDate = s
			}
			if s, ok := e["last_intentional_bump"].(string); ok {
				b.LastIntentionalBump = s
			}
			baselines = append(baselines, b)
		}
		out[k] = baselines
	}
	return out, nil
}

// LookupBaseline returns the Baseline matching (key, fixtureID) from
// the parsed map, or false if absent. Exposed for tooling
// (fixtures-rebaseline) and tests.
func LookupBaseline(baselines map[string][]Baseline, key, fixtureID string) (Baseline, bool) {
	entries, ok := baselines[key]
	if !ok {
		return Baseline{}, false
	}
	for _, b := range entries {
		if b.Fixture == fixtureID {
			return b, true
		}
	}
	return Baseline{}, false
}

// fataler is the subset of *testing.T that AssertWithinBaseline
// touches. Defined as an interface so tests can pass a recorder that
// captures Fatalf without runtime.Goexit-ing the test goroutine.
//
// *testing.T satisfies this interface — production callers pass it
// directly; tests pass a stub.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// AssertWithinBaseline checks measured wall-time against the recorded
// baseline for the (key, fixtureID) pair in baselines.toml.
//
// Behavior per ADR-0019 D.6:
//   - absent baseline: t.Fatalf with a recommended rebaseline command
//     (the gate refuses to silently pass when no anchor exists)
//   - measured <= baseline * (1 + tolerance_pct/100): pass; baseline
//     is NOT auto-updated (fixed-anchor policy)
//   - measured >  ceiling: t.Fatalf with both numbers AND a recommended
//     `task fixtures:rebaseline` command so an intentional regression
//     can be explicitly anchored with an audit-trail justification
func AssertWithinBaseline(t *testing.T, key, fixtureID string, measured time.Duration) {
	t.Helper()
	baselines, err := loadBaselines()
	if err != nil {
		t.Fatalf("AssertWithinBaseline(%q, %q): load baselines: %v", key, fixtureID, err)
	}
	assertWithinBaselineFromMap(t, baselines, key, fixtureID, measured)
}

// assertWithinBaselineFromMap is the testable core. The public helper
// loads via the package-level cache; tests build a map directly and
// pass a fataler stub to observe Fatalf without aborting the test
// goroutine.
func assertWithinBaselineFromMap(
	t fataler,
	baselines map[string][]Baseline,
	key, fixtureID string,
	measured time.Duration,
) {
	t.Helper()
	b, ok := LookupBaseline(baselines, key, fixtureID)
	if !ok {
		t.Fatalf(
			"AssertWithinBaseline(%q, %q): no baseline recorded — "+
				"run `task fixtures:rebaseline key=%s fixture=%s wall_ms=%d justification=\"initial anchor\"` first",
			key, fixtureID, key, fixtureID, measured.Milliseconds(),
		)
		return
	}
	ceilingMs := float64(b.WallMs) * (1.0 + float64(b.TolerancePct)/100.0)
	measuredMs := float64(measured.Microseconds()) / 1000.0
	if measuredMs > ceilingMs {
		t.Fatalf(
			"AssertWithinBaseline(%q, %q): measured %.1fms exceeds ceiling %.1fms "+
				"(baseline %dms +%d%% tolerance, anchored_at=%s on %s). "+
				"If this regression is intentional, run: "+
				"`task fixtures:rebaseline key=%s fixture=%s wall_ms=%d justification=\"<reason>\"`",
			key, fixtureID,
			measuredMs, ceilingMs,
			b.WallMs, b.TolerancePct, b.AnchoredAt, b.AnchoredAtDate,
			key, fixtureID, int(measuredMs+0.5),
		)
		return
	}
	t.Logf(
		"baseline OK: %s/%s measured %.1fms within ceiling %.1fms (baseline %dms +%d%%)",
		key, fixtureID, measuredMs, ceilingMs, b.WallMs, b.TolerancePct,
	)
}
