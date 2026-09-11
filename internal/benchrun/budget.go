package benchrun

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Budget is the cold-path envelope: what a first `mache build` is allowed to
// cost on the machine most people actually have (mache-2de6c0). The three
// byte limits are HARD — a run that exceeds any of them fails. Wall time is
// advisory and can never fail a run: timing gates flake on shared CI runners,
// and a gate that flakes gets disabled, which is worse than no gate.
type Budget struct {
	// PeakRSSBytes is the memory ceiling. Above it a 16 GB laptop with an
	// IDE open starts paging, and the wall time stops meaning anything.
	PeakRSSBytes int64 `toml:"peak_rss_bytes"`
	// LeylineDBBytes caps the intermediate parse artifact.
	LeylineDBBytes int64 `toml:"leyline_db_bytes"`
	// ProjectionDBBytes caps the artifact the user keeps.
	ProjectionDBBytes int64 `toml:"projection_db_bytes"`
	// WallMsAdvisory is reported against, never enforced.
	WallMsAdvisory int `toml:"wall_ms_advisory"`
}

// Observed is what a bench run measured.
type Observed struct {
	PeakRSSBytes      int64
	LeylineDBBytes    int64
	ProjectionDBBytes int64
	Wall              time.Duration
}

// Violation is one budget line that was exceeded.
type Violation struct {
	Metric string
	Got    int64
	Limit  int64
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s exceeds the %s budget by %s",
		v.Metric, HumanBytes(v.Got), HumanBytes(v.Limit), HumanBytes(v.Got-v.Limit))
}

// Check reports every hard limit o exceeds, in a fixed order so two runs are
// diffable. An empty result means the run is within budget.
//
// Wall time is deliberately absent: see Budget.
func (b Budget) Check(o Observed) []Violation {
	var out []Violation
	for _, c := range []struct {
		metric string
		got    int64
		limit  int64
	}{
		{"peak RSS", o.PeakRSSBytes, b.PeakRSSBytes},
		{"leyline db", o.LeylineDBBytes, b.LeylineDBBytes},
		{"projection db", o.ProjectionDBBytes, b.ProjectionDBBytes},
	} {
		if c.got > c.limit {
			out = append(out, Violation{Metric: c.metric, Got: c.got, Limit: c.limit})
		}
	}
	return out
}

// budgetFile is the on-disk shape of the committed budget.
type budgetFile struct {
	Schema string `toml:"schema"`
	Budget Budget `toml:"budget"`
}

// LoadBudget reads the committed budget. A limit that is missing or
// non-positive is an ERROR, not a pass: a zero ceiling read as "no limit"
// would turn a typo into a permanently green gate, which is the one failure
// mode a budget file must not have.
func LoadBudget(path string) (Budget, error) {
	var f budgetFile
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return Budget{}, fmt.Errorf("read budget %s: %w", path, err)
	}
	b := f.Budget
	for _, c := range []struct {
		name  string
		value int64
	}{
		{"peak_rss_bytes", b.PeakRSSBytes},
		{"leyline_db_bytes", b.LeylineDBBytes},
		{"projection_db_bytes", b.ProjectionDBBytes},
	} {
		if c.value <= 0 {
			return Budget{}, fmt.Errorf("budget %s: %s is %d; every limit must be positive "+
				"(a zero limit would read as 'unlimited' and green the gate forever)",
				path, c.name, c.value)
		}
	}
	return b, nil
}
