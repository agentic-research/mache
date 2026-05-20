//! Flat-lattice algebra with explicit `⊤ = Conflict { values, sources }`.
//!
//! Bead `obs-registry-and-fold` (rosary-980824). ADR-0010 invariant 10
//! (`cross_source_status_conflict_is_top`).
//!
//! Used **only** for cross-source [`crate::observation::FieldName::Status`]
//! derivation — NOT as a primitive field algebra. The substrate's
//! per-field algebras (chain-max, LWW, OR-set) handle individual fields
//! per source. A separate "derive status" pass over those per-source
//! results uses *this* algebra to detect cross-source disagreement.
//!
//! Concrete example: Linear says `Done` for a bead, GitHub PR for the
//! same bead is `closed-without-merge`. The substrate's per-field fold
//! gives:
//!
//! - `status@linear   = "Done"`
//! - `status@github   = "Closed-Unmerged"`
//!
//! These get fed to this flat lattice → `⊤(Conflict)` with witnesses
//! `["Done"@linear, "Closed-Unmerged"@github]`. The user-facing
//! `rsry status` surfaces the conflict instead of silently picking one.

use anyhow::Result;
use serde::{Deserialize, Serialize};

use super::Source;

/// Flat-lattice element: bottom (no observations), one value, or top
/// (conflict — multiple distinct values seen).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum FlatLattice<T> {
    /// No observations — analogous to `Bot`.
    Empty,
    /// All observations agree on this value.
    Single(T),
    /// Distinct values observed across sources. The witnesses are
    /// kept in `(value, source)` pairs sorted lexicographically by
    /// source so equality is order-independent.
    Top { witnesses: Vec<(T, Source)> },
}

impl<T: Clone + Eq + Ord> FlatLattice<T> {
    /// True iff the lattice is the conflict element (`⊤`).
    pub fn is_conflict(&self) -> bool {
        matches!(self, FlatLattice::Top { .. })
    }

    /// True iff the lattice has at least one observation.
    pub fn is_present(&self) -> bool {
        !matches!(self, FlatLattice::Empty)
    }
}

/// Reduce a list of `(value, source)` per-source observations into a
/// single flat-lattice element. The function is total + deterministic
/// regardless of input order — sources are sorted internally before
/// any equality comparison so reorder invariance holds.
///
/// The algebra is associative and commutative on the SET of inputs
/// (ADR-0010 invariant 14), and idempotent (joining the same value
/// from the same source twice is the same as once — same result).
pub fn join_per_source<T: Clone + Eq + Ord>(per_source: &[(T, Source)]) -> Result<FlatLattice<T>> {
    if per_source.is_empty() {
        return Ok(FlatLattice::Empty);
    }

    // Dedupe: same (value, source) pair contributes once.
    let mut sorted: Vec<(T, Source)> = per_source.to_vec();
    sorted.sort();
    sorted.dedup();

    // Distinct values?
    let first_value = sorted[0].0.clone();
    let all_agree = sorted.iter().all(|(v, _)| v == &first_value);

    if all_agree {
        Ok(FlatLattice::Single(first_value))
    } else {
        // Top — keep ALL witnesses (including agreeing ones) so the
        // conflict surface shows the full picture, not just the diff.
        Ok(FlatLattice::Top { witnesses: sorted })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(name: &str) -> Source {
        Source::new(name)
    }

    #[test]
    fn empty_input_is_empty_lattice() {
        let r: FlatLattice<String> = join_per_source(&[]).unwrap();
        assert_eq!(r, FlatLattice::Empty);
        assert!(!r.is_present());
        assert!(!r.is_conflict());
    }

    #[test]
    fn all_agree_single() {
        let r = join_per_source(&[
            ("Done".to_string(), s("linear")),
            ("Done".to_string(), s("github")),
        ])
        .unwrap();
        assert_eq!(r, FlatLattice::Single("Done".to_string()));
        assert!(r.is_present());
        assert!(!r.is_conflict());
    }

    /// ADR-0010 invariant 10: cross_source_status_conflict_is_top.
    #[test]
    fn distinct_values_become_top_with_witnesses() {
        let r = join_per_source(&[
            ("Done".to_string(), s("linear")),
            ("Closed-Unmerged".to_string(), s("github")),
        ])
        .unwrap();
        assert!(r.is_conflict());
        let witnesses = match r {
            FlatLattice::Top { witnesses } => witnesses,
            _ => panic!("expected Top"),
        };
        // Witnesses include both — full picture, not just the diff.
        assert_eq!(witnesses.len(), 2);
        let labels: Vec<&str> = witnesses.iter().map(|(v, _)| v.as_str()).collect();
        assert!(labels.contains(&"Done"));
        assert!(labels.contains(&"Closed-Unmerged"));
    }

    #[test]
    fn idempotent_under_dedup() {
        // Same (value, source) twice should be equivalent to once.
        let twice = join_per_source(&[
            ("Done".to_string(), s("linear")),
            ("Done".to_string(), s("linear")),
        ])
        .unwrap();
        let once = join_per_source(&[("Done".to_string(), s("linear"))]).unwrap();
        assert_eq!(twice, once);
    }

    #[test]
    fn reorder_invariant() {
        let inputs = vec![
            ("Done".to_string(), s("linear")),
            ("Closed-Unmerged".to_string(), s("github")),
            ("Pending".to_string(), s("git")),
        ];
        let r1 = join_per_source(&inputs).unwrap();
        let mut reversed = inputs.clone();
        reversed.reverse();
        let r2 = join_per_source(&reversed).unwrap();
        assert_eq!(r1, r2, "result must be order-independent");
    }

    #[test]
    fn top_absorbs_under_more_distinct_values() {
        // Already Top with 2 witnesses; adding a 3rd distinct value
        // keeps it Top with 3 witnesses (all preserved).
        let r = join_per_source(&[
            ("A".to_string(), s("src1")),
            ("B".to_string(), s("src2")),
            ("C".to_string(), s("src3")),
        ])
        .unwrap();
        let witnesses = match r {
            FlatLattice::Top { witnesses } => witnesses,
            _ => panic!(),
        };
        assert_eq!(witnesses.len(), 3);
    }
}
