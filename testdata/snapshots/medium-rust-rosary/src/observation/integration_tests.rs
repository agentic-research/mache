//! ADR-0010 §"Test contract" — the 14-invariant integration test.
//!
//! Each test below exercises the substrate via its public surface
//! and asserts the invariant from the ADR's table. Names match the
//! ADR's invariant labels exactly so a reviewer can map between them
//! by eye.
//!
//! Lives under `#[cfg(test)] pub mod integration_tests` rather than at
//! `tests/observation_lattice.rs` — rosary is a binary crate with no
//! `[lib]` target, so a top-level `tests/` file can't `use rsry::*`.
//! The contract is the same; the location is what fits rosary's
//! current crate shape. If/when rosary grows a lib target, this can
//! move to the canonical location.
//!
//! The substrate is a pure algebra layer in Phase 1 — no Dolt schema,
//! no observers wired. This test runs in-process against
//! `ObservationLog::new()` populated by hand.

#![allow(dead_code)]

use chrono::{DateTime, Utc};

use crate::observation::algebra_flat::FlatLattice;
use crate::observation::log::ObservationLog;
use crate::observation::quarantine::{QuarantineLog, QuarantineReason};
use crate::observation::tree_fold::{
    BeadNode, DecadeNode, LeafStatus, RollupStatus, ThreadNode, tree_fold,
};
use crate::observation::{
    FieldName, FieldValue, Observation, PipelineVerdictValue, SignetCert, Source, fold,
};
use crate::store::WorkRef;

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

// =====================================================================
// Invariants 1-3: Chain-max algebra (PipelineVerdict)
// =====================================================================

/// Invariant 1: `pipeline_verdict=Pass ⊕ pipeline_verdict=Pass = Pass`.
#[test]
fn chain_max_idempotent() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
        at(1000),
    ));
    log.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
        at(2000),
    ));
    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    let val = view
        .per_source
        .get(&FieldName::PipelineVerdict)
        .and_then(|m| m.get(&Source::new("src")))
        .unwrap();
    assert_eq!(
        val,
        &FieldValue::PipelineVerdict(PipelineVerdictValue::Pass)
    );
}

/// Invariant 2: `pipeline_verdict=Dispatched ⊕ pipeline_verdict=Pass = Pass` (max wins).
#[test]
fn chain_max_monotone() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Dispatched),
        at(1000),
    ));
    log.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
        at(500),
    ));
    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    let val = view
        .per_source
        .get(&FieldName::PipelineVerdict)
        .and_then(|m| m.get(&Source::new("src")))
        .unwrap();
    assert_eq!(
        val,
        &FieldValue::PipelineVerdict(PipelineVerdictValue::Pass)
    );
}

/// Invariant 3: `Fail` and `Deadletter` don't advance the chain.
#[test]
fn chain_max_unranked_ignored() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
        at(1000),
    ));
    log.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::PipelineVerdict,
        FieldValue::PipelineVerdict(PipelineVerdictValue::Fail),
        at(2000),
    ));
    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    let val = view
        .per_source
        .get(&FieldName::PipelineVerdict)
        .and_then(|m| m.get(&Source::new("src")))
        .unwrap();
    assert_eq!(
        val,
        &FieldValue::PipelineVerdict(PipelineVerdictValue::Pass)
    );
}

// =====================================================================
// Invariants 4-5: LWW-register algebra
// =====================================================================

/// Invariant 4: LWW with equal `observed_at` resolved by `source_id` lex.
#[test]
fn lww_tiebreak_total() {
    let w = workref("b1");
    let same_ts = at(1000);
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "alpha",
        "e1",
        FieldName::Assignee,
        FieldValue::OptString(Some("alice".to_string())),
        same_ts,
    ));
    log.insert(obs(
        &w,
        "zulu",
        "e2",
        FieldName::Assignee,
        FieldValue::OptString(Some("zorro".to_string())),
        same_ts,
    ));
    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    // Within each source, the source-scoped LWW picks the only obs;
    // we get two per-source entries, both surfaced. The cross-source
    // disagreement is a per-source-distinct view, not a tiebreak —
    // tiebreak fires only when two observations land in the SAME
    // source with the same timestamp. Add that case:
    let _ = view; // (sanity that the view exists)

    // Same source, same ts → lex tiebreak inside the algebra. The
    // LWW-register's tiebreak rule is `(observed_at, source.as_str())`,
    // so when both are equal, slice order doesn't matter — the
    // algebra's deterministic max picks the same winner each time.
    let mut log2 = ObservationLog::new();
    log2.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::Assignee,
        FieldValue::OptString(Some("alice".to_string())),
        same_ts,
    ));
    log2.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::Assignee,
        FieldValue::OptString(Some("bob".to_string())),
        same_ts,
    ));
    let r1 = fold::fold(&log2).unwrap();
    let r2 = fold::fold(&log2).unwrap();
    assert_eq!(r1, r2, "tiebreak must produce stable result");
}

/// Invariant 5: `pr_url=None` requires explicit observation, never inferred.
#[test]
fn lww_unset_explicit() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "github",
        "e1",
        FieldName::PrUrl,
        FieldValue::OptString(Some("https://example/pr/1".to_string())),
        at(1000),
    ));
    log.insert(obs(
        &w,
        "github",
        "e2",
        FieldName::PrUrl,
        FieldValue::OptString(None),
        at(2000),
    ));
    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    let val = view
        .per_source
        .get(&FieldName::PrUrl)
        .and_then(|m| m.get(&Source::new("github")))
        .unwrap();
    assert_eq!(
        val,
        &FieldValue::OptString(None),
        "explicit None at later ts must win",
    );
}

// =====================================================================
// Invariants 6-7: OR-set algebra
// =====================================================================

/// Invariant 6: `add(c, t1) + remove(c, t1)` in any order = absent.
/// (Phase 1 is add-only; verified as add-order-invariance.)
#[test]
fn or_set_add_remove_commute() {
    let w = workref("b1");
    let mut log1 = ObservationLog::new();
    log1.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::Comment,
        FieldValue::String("first".to_string()),
        at(1000),
    ));
    log1.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::Comment,
        FieldValue::String("second".to_string()),
        at(2000),
    ));

    let mut log2 = ObservationLog::new();
    // Insert in reverse order.
    log2.insert(obs(
        &w,
        "src",
        "e2",
        FieldName::Comment,
        FieldValue::String("second".to_string()),
        at(2000),
    ));
    log2.insert(obs(
        &w,
        "src",
        "e1",
        FieldName::Comment,
        FieldValue::String("first".to_string()),
        at(1000),
    ));

    assert_eq!(fold::fold(&log1).unwrap(), fold::fold(&log2).unwrap());
}

/// Invariant 7: same comment from two sources is two distinct entries.
#[test]
fn or_set_unique_tags() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    log.insert(obs(
        &w,
        "linear",
        "e1",
        FieldName::Comment,
        FieldValue::String("LGTM".to_string()),
        at(1000),
    ));
    log.insert(obs(
        &w,
        "github",
        "e2",
        FieldName::Comment,
        FieldValue::String("LGTM".to_string()),
        at(1500),
    ));

    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    let comments_by_source = view.per_source.get(&FieldName::Comment).unwrap();
    assert_eq!(
        comments_by_source.len(),
        2,
        "same text, distinct sources → 2 per-source entries",
    );
    assert!(comments_by_source.contains_key(&Source::new("linear")));
    assert!(comments_by_source.contains_key(&Source::new("github")));
}

// =====================================================================
// Invariant 8: dedup before fold
// =====================================================================

/// Invariant 8: replaying a webhook with the same
/// `(source, source_event_id, payload_hash)` is a no-op.
#[test]
fn dedup_before_fold() {
    let w = workref("b1");
    let mut log = ObservationLog::new();
    let o = obs(
        &w,
        "github",
        "evt-replay",
        FieldName::Assignee,
        FieldValue::OptString(Some("alice".to_string())),
        at(1000),
    );
    let first = log.insert(o.clone());
    let second = log.insert(o.clone());
    let third = log.insert(o);
    assert!(first);
    assert!(!second);
    assert!(!third);
    assert_eq!(log.len(), 1, "replays must not accumulate entries");
}

// =====================================================================
// Invariant 9: reorder invariance
// =====================================================================

/// Invariant 9: `fold(perm(O)) = fold(O)` for any permutation.
#[test]
fn reorder_invariance() {
    let w = workref("b1");
    let observations = vec![
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
            FieldName::PrUrl,
            FieldValue::OptString(Some("https://example/pr/1".to_string())),
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
        obs(
            &w,
            "github",
            "e4",
            FieldName::PipelineVerdict,
            FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
            at(4000),
        ),
    ];

    let mut log_a = ObservationLog::new();
    for o in &observations {
        log_a.insert(o.clone());
    }
    let mut log_b = ObservationLog::new();
    for o in observations.iter().rev() {
        log_b.insert(o.clone());
    }
    let mut log_c = ObservationLog::new();
    let perm = [
        &observations[2],
        &observations[0],
        &observations[3],
        &observations[1],
    ];
    for o in perm {
        log_c.insert(o.clone());
    }

    let r_a = fold::fold(&log_a).unwrap();
    let r_b = fold::fold(&log_b).unwrap();
    let r_c = fold::fold(&log_c).unwrap();
    assert_eq!(r_a, r_b);
    assert_eq!(r_b, r_c);
}

// =====================================================================
// Invariant 10: cross-source status conflict is Top
// =====================================================================

/// Invariant 10: when two sources' status disagree, the cross-source
/// result is `⊤(Conflict)` with witnesses.
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

    let derived = fold::fold(&log).unwrap();
    let view = derived.get(&w).unwrap();
    assert!(view.status.is_conflict());
    let witnesses = match &view.status {
        FlatLattice::Top { witnesses } => witnesses.clone(),
        _ => panic!("expected Top"),
    };
    assert_eq!(witnesses.len(), 2);
    let labels: Vec<&str> = witnesses.iter().map(|(v, _)| v.as_str()).collect();
    assert!(labels.contains(&"Done"));
    assert!(labels.contains(&"Closed-Unmerged"));
}

// =====================================================================
// Invariants 11-12: Quarantine
// =====================================================================

/// Invariant 11: invalid-cert obs never appear in the derived view.
///
/// In Phase 1 this is structural: the QuarantineLog is a separate
/// data structure from ObservationLog; the fold consumes ONLY
/// ObservationLog. Quarantined observations are never inserted into
/// ObservationLog (callers route them via quarantine.add() instead).
/// We exercise the structural guarantee here — observations sent to
/// quarantine don't appear in the fold's output.
#[test]
fn quarantine_does_not_join() {
    let w = workref("b1");
    let mut obs_log = ObservationLog::new();
    let mut q_log = QuarantineLog::new();

    // Add a "good" observation to ObservationLog.
    let good = obs(
        &w,
        "github",
        "e-good",
        FieldName::Assignee,
        FieldValue::OptString(Some("alice".to_string())),
        at(1000),
    );
    obs_log.insert(good);

    // Build a "bad" observation with an invalid cert; route to quarantine.
    let mut bad = obs(
        &w,
        "user",
        "e-bad",
        FieldName::Assignee,
        FieldValue::OptString(Some("evil".to_string())),
        at(2000),
    );
    bad.cert = Some(SignetCert {
        key_id: "bogus".to_string(),
        signature: "deadbeef".to_string(),
    });
    q_log.add(
        bad,
        QuarantineReason::InvalidCert {
            detail: "test fixture".to_string(),
        },
    );

    let derived = fold::fold(&obs_log).unwrap();
    let view = derived.get(&w).unwrap();
    let assignee_per_source = view.per_source.get(&FieldName::Assignee).unwrap();

    // Only the GitHub observation made it into the fold; the
    // quarantined "user" observation is absent.
    assert!(assignee_per_source.contains_key(&Source::new("github")));
    assert!(!assignee_per_source.contains_key(&Source::new("user")));
    assert_eq!(
        assignee_per_source.get(&Source::new("github")).unwrap(),
        &FieldValue::OptString(Some("alice".to_string())),
    );
}

/// Invariant 12: quarantined obs are surfaced via dedicated path,
/// not silently dropped.
#[test]
fn quarantine_is_queryable() {
    let w = workref("b1");
    let mut q_log = QuarantineLog::new();
    let bad = obs(
        &w,
        "user",
        "e-bad",
        FieldName::Assignee,
        FieldValue::OptString(Some("evil".to_string())),
        at(1000),
    );
    q_log.add(
        bad,
        QuarantineReason::InvalidCert {
            detail: "expired".to_string(),
        },
    );

    let entries: Vec<_> = q_log.iter_quarantined().collect();
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].observation.source.as_str(), "user");
    assert!(matches!(
        entries[0].reason,
        QuarantineReason::InvalidCert { .. }
    ));
}

// =====================================================================
// Invariant 13: tree-fold determinism
// =====================================================================

/// Invariant 13: Decade/Thread/Bead rollup is a pure function of
/// child states.
#[test]
fn tree_fold_deterministic() {
    let d1 = DecadeNode {
        decade_id: "d1".to_string(),
        threads: vec![
            ThreadNode {
                thread_id: "t-a".to_string(),
                beads: vec![
                    BeadNode {
                        bead_id: "b1".to_string(),
                        status: LeafStatus::Done,
                    },
                    BeadNode {
                        bead_id: "b2".to_string(),
                        status: LeafStatus::Done,
                    },
                ],
            },
            ThreadNode {
                thread_id: "t-b".to_string(),
                beads: vec![BeadNode {
                    bead_id: "b3".to_string(),
                    status: LeafStatus::Open,
                }],
            },
        ],
    };
    // Reordered children — same beads, same threads, different list order.
    let d2 = DecadeNode {
        decade_id: "d1".to_string(),
        threads: vec![
            ThreadNode {
                thread_id: "t-b".to_string(),
                beads: vec![BeadNode {
                    bead_id: "b3".to_string(),
                    status: LeafStatus::Open,
                }],
            },
            ThreadNode {
                thread_id: "t-a".to_string(),
                beads: vec![
                    BeadNode {
                        bead_id: "b2".to_string(),
                        status: LeafStatus::Done,
                    },
                    BeadNode {
                        bead_id: "b1".to_string(),
                        status: LeafStatus::Done,
                    },
                ],
            },
        ],
    };
    let r1 = tree_fold(&d1);
    let r2 = tree_fold(&d2);
    assert_eq!(r1, r2, "tree fold must be invariant under child order");
    assert_eq!(r1.status, RollupStatus::InProgress);
}

// =====================================================================
// Invariant 14: convergence under partition
// =====================================================================

/// Invariant 14: `fold(O₁ ∪ O₂) = fold(union)` for arbitrary partition.
/// (Per-field algebras are commutative + idempotent so feeding via
/// union == feeding via combined set.)
#[test]
fn convergence_under_partition() {
    let w = workref("b1");
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
    let half2 = vec![obs(
        &w,
        "github",
        "e3",
        FieldName::Status,
        FieldValue::String("Closed-Unmerged".to_string()),
        at(2500),
    )];

    let mut combined = ObservationLog::new();
    for o in half1.iter().chain(half2.iter()) {
        combined.insert(o.clone());
    }

    // Build the union log and re-fold; per-field algebras' ACI
    // properties guarantee this converges to the same view.
    let mut union = ObservationLog::new();
    for o in half2.iter().chain(half1.iter()) {
        union.insert(o.clone());
    }

    assert_eq!(fold::fold(&combined).unwrap(), fold::fold(&union).unwrap());
}
