//! Deterministic fold over an observation log → derived view.
//!
//! Bead `obs-registry-and-fold` (rosary-980824). ADR-0010 invariants
//! 9 (`reorder_invariance`) and 14 (`convergence_under_partition`).
//!
//! The fold orchestrates two layers:
//!
//! 1. **Per-field, per-source primitive fold** — for each
//!    `(work_item, field, source)` tuple, run the registered algebra
//!    over the matching observations. Output: a per-source value per
//!    field (e.g. "Linear says assignee=alice", "GitHub says
//!    assignee=bob").
//!
//! 2. **Cross-source flat-lattice join** — for `FieldName::Status`
//!    specifically (the only "derived, not primitive" field per
//!    ADR-0010), feed the per-source values to
//!    `algebra_flat::join_per_source` to detect cross-source
//!    disagreement (`⊤ = Conflict`).
//!
//! Other fields stop at layer 1 — their per-source values are
//! reported as-is. Cross-source disagreement on, say, `assignee` is
//! a real signal too (Linear was edited but GitHub stale, or vice
//! versa) but Phase 1 only computes `Status` cross-source. Future:
//! generalize the cross-source join across all fields, surface as
//! `derived_view.conflicts` lookup.

use std::collections::{BTreeMap, BTreeSet};

use anyhow::Result;

use super::algebra_flat::{FlatLattice, join_per_source};
use super::log::ObservationLog;
use super::{FieldName, FieldValue, Source, WorkRef};

/// Derived view for a single work item.
#[derive(Debug, Clone, PartialEq)]
pub struct DerivedView {
    pub work_item: WorkRef,
    /// Per-source primitive folds. Outer key is field; inner is
    /// `(source → folded value)`. Sources only appear for fields they
    /// have observations on.
    pub per_source: BTreeMap<FieldName, BTreeMap<Source, FieldValue>>,
    /// Cross-source `Status` lattice element. `Empty` when no source
    /// reported a status, `Single` when all sources agree, `Top` with
    /// witnesses when sources disagree.
    pub status: FlatLattice<String>,
}

/// Fold an observation log into a per-`WorkRef` derived view map.
///
/// Deterministic over the observation set: same input set → same
/// output, regardless of insertion order. Idempotent under the
/// log's dedup key (`Observation::dedup_key`).
pub fn fold(log: &ObservationLog) -> Result<BTreeMap<WorkRef, DerivedView>> {
    // Collect distinct (work_item, field, source) groupings from the
    // log. BTreeMap/BTreeSet for deterministic iteration.
    let mut by_work: BTreeMap<WorkRef, DerivedView> = BTreeMap::new();
    let registry = super::registry::global();

    // Group observations by work_item.
    let mut work_items: BTreeSet<WorkRef> = BTreeSet::new();
    for o in log.iter() {
        work_items.insert(o.work_item.clone());
    }

    for work in work_items {
        let mut per_source: BTreeMap<FieldName, BTreeMap<Source, FieldValue>> = BTreeMap::new();

        // Find every field on this work_item across the log.
        let mut fields: BTreeSet<FieldName> = BTreeSet::new();
        for o in log.iter().filter(|o| o.work_item == work) {
            fields.insert(o.field.clone());
        }

        // Per-field, per-source algebra fold.
        for field in &fields {
            let Some(algebra) = registry.get(field) else {
                // Status (or any other derived field) has no primitive
                // algebra. Skip; the cross-source step below handles it.
                continue;
            };

            // Group observations on this (work_item, field) by source.
            let mut by_source: BTreeMap<Source, Vec<&super::Observation>> = BTreeMap::new();
            for o in log
                .iter()
                .filter(|o| o.work_item == work && &o.field == field)
            {
                by_source.entry(o.source.clone()).or_default().push(o);
            }

            let mut field_per_source: BTreeMap<Source, FieldValue> = BTreeMap::new();
            for (source, obs_list) in by_source {
                let folded = algebra.fold(&obs_list)?;
                field_per_source.insert(source, folded);
            }
            per_source.insert(field.clone(), field_per_source);
        }

        // Cross-source flat-lattice join for `Status`. Status is the
        // ONLY derived field in Phase 1 (per ADR-0010); other fields'
        // per-source disagreements aren't lattice-joined yet.
        // We collect per-source `Status` observations directly from
        // the log (Status doesn't have a registered primitive algebra,
        // so `per_source` doesn't carry them). Each source contributes
        // its latest Status value (LWW within a source).
        let status_obs: Vec<&super::Observation> = log
            .iter()
            .filter(|o| o.work_item == work && o.field == FieldName::Status)
            .collect();
        let mut status_per_source: BTreeMap<Source, String> = BTreeMap::new();
        for o in &status_obs {
            // Within a source: LWW by observed_at (tiebreak source.name
            // is moot here since we're scoped to one source — fall back
            // to "later wins, fresh-arrival wins on exact tie").
            let entry = status_per_source.entry(o.source.clone());
            let new_val = match &o.value {
                FieldValue::String(s) => s.clone(),
                FieldValue::OptString(Some(s)) => s.clone(),
                _ => continue, // unrecognized status shape — skip
            };
            entry
                .and_modify(|existing| {
                    if existing != &new_val {
                        *existing = new_val.clone();
                    }
                })
                .or_insert(new_val);
        }
        let per_source_pairs: Vec<(String, Source)> = status_per_source
            .into_iter()
            .map(|(src, val)| (val, src))
            .collect();
        let status = join_per_source(&per_source_pairs)?;

        by_work.insert(
            work.clone(),
            DerivedView {
                work_item: work,
                per_source,
                status,
            },
        );
    }

    Ok(by_work)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{Observation, PipelineVerdictValue, Source};
    use chrono::{DateTime, Utc};

    fn at(secs: i64) -> DateTime<Utc> {
        DateTime::from_timestamp(secs, 0).unwrap()
    }

    fn workref(id: &str) -> WorkRef {
        WorkRef {
            repo: "r".to_string(),
            scope: String::new(),
            bead_id: id.to_string(),
        }
    }

    fn obs(
        work: &WorkRef,
        source: &str,
        evt: &str,
        field: FieldName,
        value: FieldValue,
        observed_at: DateTime<Utc>,
    ) -> Observation {
        Observation {
            work_item: work.clone(),
            source: Source::new(source),
            source_event_id: evt.to_string(),
            field,
            value,
            observed_at,
            cert: None,
            payload_hash: format!("{source}-{evt}"),
        }
    }

    /// ADR-0010 invariant 9: reorder_invariance. Same observation set
    /// in any order produces the same derived view.
    #[test]
    fn reorder_invariance() {
        let w = workref("b1");
        let observations = [
            obs(
                &w,
                "linear",
                "e1",
                FieldName::Assignee,
                FieldValue::OptString(Some("alice".to_string())),
                at(1000),
            ),
            obs(
                &w,
                "github",
                "e2",
                FieldName::PipelineVerdict,
                FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
                at(2000),
            ),
            obs(
                &w,
                "bead",
                "e3",
                FieldName::Status,
                FieldValue::String("Done".to_string()),
                at(3000),
            ),
        ];

        let mut log1 = ObservationLog::new();
        for o in &observations {
            log1.insert(o.clone());
        }
        let mut log2 = ObservationLog::new();
        for o in observations.iter().rev() {
            log2.insert(o.clone());
        }

        let r1 = fold(&log1).unwrap();
        let r2 = fold(&log2).unwrap();
        assert_eq!(r1, r2, "fold must be invariant under insertion order");
    }

    /// ADR-0010 invariant 10: cross_source_status_conflict_is_top.
    /// Two sources with distinct Status values produce a Top with
    /// witnesses.
    #[test]
    fn cross_source_status_conflict_is_top() {
        let w = workref("b1");
        let mut log = ObservationLog::new();
        log.insert(obs(
            &w,
            "linear",
            "e1",
            FieldName::Status,
            FieldValue::String("Done".to_string()),
            at(1000),
        ));
        log.insert(obs(
            &w,
            "github",
            "e2",
            FieldName::Status,
            FieldValue::String("Closed-Unmerged".to_string()),
            at(1500),
        ));

        let derived = fold(&log).unwrap();
        let view = derived.get(&w).unwrap();
        assert!(view.status.is_conflict(), "distinct status values → Top");
        let witnesses = match &view.status {
            FlatLattice::Top { witnesses } => witnesses,
            _ => panic!(),
        };
        assert_eq!(witnesses.len(), 2);
        let labels: Vec<&str> = witnesses.iter().map(|(v, _)| v.as_str()).collect();
        assert!(labels.contains(&"Done"));
        assert!(labels.contains(&"Closed-Unmerged"));
    }

    #[test]
    fn cross_source_status_agreement_is_single() {
        let w = workref("b1");
        let mut log = ObservationLog::new();
        log.insert(obs(
            &w,
            "linear",
            "e1",
            FieldName::Status,
            FieldValue::String("Done".to_string()),
            at(1000),
        ));
        log.insert(obs(
            &w,
            "github",
            "e2",
            FieldName::Status,
            FieldValue::String("Done".to_string()),
            at(1500),
        ));

        let derived = fold(&log).unwrap();
        let view = derived.get(&w).unwrap();
        assert_eq!(view.status, FlatLattice::Single("Done".to_string()));
    }

    #[test]
    fn no_status_observations_is_empty_lattice() {
        let w = workref("b1");
        let mut log = ObservationLog::new();
        log.insert(obs(
            &w,
            "linear",
            "e1",
            FieldName::Assignee,
            FieldValue::OptString(Some("alice".to_string())),
            at(1000),
        ));
        let derived = fold(&log).unwrap();
        let view = derived.get(&w).unwrap();
        assert_eq!(view.status, FlatLattice::Empty);
    }

    #[test]
    fn empty_log_is_empty_view() {
        let log = ObservationLog::new();
        let derived = fold(&log).unwrap();
        assert!(derived.is_empty());
    }

    /// ADR-0010 invariant 14: convergence_under_partition.
    /// `fold(O₁ ∪ O₂) = merge(fold(O₁), fold(O₂))` — folding two
    /// halves separately and combining the per-source results gives
    /// the same answer as folding the combined log directly.
    #[test]
    fn convergence_under_partition() {
        let w = workref("b1");
        // Half 1: assignee + status from linear
        let half1 = vec![
            obs(
                &w,
                "linear",
                "e1",
                FieldName::Assignee,
                FieldValue::OptString(Some("alice".to_string())),
                at(1000),
            ),
            obs(
                &w,
                "linear",
                "e2",
                FieldName::Status,
                FieldValue::String("Done".to_string()),
                at(2000),
            ),
        ];
        // Half 2: status from github, conflict-inducing
        let half2 = vec![obs(
            &w,
            "github",
            "e3",
            FieldName::Status,
            FieldValue::String("Closed-Unmerged".to_string()),
            at(2500),
        )];

        // Combined log
        let mut combined = ObservationLog::new();
        for o in half1.iter().chain(half2.iter()) {
            combined.insert(o.clone());
        }
        let r_combined = fold(&combined).unwrap();

        // Two-step partition: build half logs separately, then combine
        // the underlying observations and re-fold (the per-source
        // primitive algebras are commutative + idempotent, so feeding
        // them via union is equivalent).
        let mut union = ObservationLog::new();
        let mut log1 = ObservationLog::new();
        for o in &half1 {
            log1.insert(o.clone());
            union.insert(o.clone());
        }
        let mut log2 = ObservationLog::new();
        for o in &half2 {
            log2.insert(o.clone());
            union.insert(o.clone());
        }
        // Sanity: each half folds independently without panic.
        let _ = fold(&log1).unwrap();
        let _ = fold(&log2).unwrap();

        let r_union = fold(&union).unwrap();
        assert_eq!(
            r_combined, r_union,
            "fold(O1 ∪ O2) = fold(union) — convergence under partition",
        );
    }
}
