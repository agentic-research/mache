//! Decade ⊃ Thread ⊃ Bead catamorphism.
//!
//! Bead `obs-tree-fold` (rosary-983757). ADR-0010 invariant 13
//! (`tree_fold_deterministic`).
//!
//! BDR's hierarchy is a **strict tree partition** — every bead lives in
//! exactly one thread, every thread in exactly one decade. No overlap,
//! no shared substructure. So this is a catamorphism (deterministic
//! bottom-up rollup), NOT a sheaf operation. ADR-0010 §"BDR
//! parent-child is a tree fold, not a sheaf" calls this out
//! explicitly: "thread is `done` iff all member beads are at terminal
//! states" is a pure function of child states.
//!
//! Phase 1 ships with input/output types and the rollup function in
//! pure form — no I/O on the rosary store yet. Phase 2 wires
//! `crate::store::HierarchyStore` to feed the input. The catamorphism
//! itself doesn't change.

use serde::{Deserialize, Serialize};

/// Aggregate status for a node in the BDR tree. Closed set so the
/// fold is total (every leaf produces one of these, every interior
/// node rolls up to one of these).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RollupStatus {
    /// At least one descendant is open / unfinished.
    InProgress,
    /// Every leaf descendant is at a terminal state (Done/Closed).
    Done,
    /// At least one descendant is hard-blocked (dependency or external).
    Blocked,
    /// No leaf descendants at all (empty thread / empty decade).
    Empty,
}

/// Leaf status — what a single bead contributes to its parent's
/// rollup. Maps from the `BeadState` enum at fold-input time so this
/// module doesn't depend on the bead crate's wire format.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LeafStatus {
    Open,
    InProgress,
    Done,
    Blocked,
}

/// One bead, leaf in the BDR tree.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BeadNode {
    pub bead_id: String,
    pub status: LeafStatus,
}

/// One thread, interior node holding ordered leaves.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ThreadNode {
    pub thread_id: String,
    pub beads: Vec<BeadNode>,
}

/// One decade, root of a sub-tree. The fold is over a single decade;
/// the orchestrator can map over multiple decades to get the full
/// system view.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DecadeNode {
    pub decade_id: String,
    pub threads: Vec<ThreadNode>,
}

/// Per-thread rollup. `beads_done / beads_total` lets consumers render
/// progress without re-walking the leaves.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ThreadRollup {
    pub thread_id: String,
    pub beads_done: usize,
    pub beads_total: usize,
    pub status: RollupStatus,
}

/// Per-decade rollup with thread breakdown.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DecadeRollup {
    pub decade_id: String,
    pub threads: Vec<ThreadRollup>,
    pub threads_done: usize,
    pub threads_total: usize,
    pub status: RollupStatus,
}

/// Roll a thread's beads up to a single status.
///
/// Rules:
/// - empty bead list → `Empty`
/// - any bead `Blocked` → thread `Blocked`
/// - all beads `Done` → thread `Done`
/// - otherwise → `InProgress`
pub fn fold_thread(thread: &ThreadNode) -> ThreadRollup {
    let total = thread.beads.len();
    let done = thread
        .beads
        .iter()
        .filter(|b| b.status == LeafStatus::Done)
        .count();

    let status = if total == 0 {
        RollupStatus::Empty
    } else if thread.beads.iter().any(|b| b.status == LeafStatus::Blocked) {
        RollupStatus::Blocked
    } else if done == total {
        RollupStatus::Done
    } else {
        RollupStatus::InProgress
    };

    ThreadRollup {
        thread_id: thread.thread_id.clone(),
        beads_done: done,
        beads_total: total,
        status,
    }
}

/// Roll a decade up. Same rules at the thread level: any `Blocked`
/// thread blocks the decade; all `Done` threads → `Done`; etc.
/// `Empty` threads count for `threads_total` (the decade has them as
/// children) but don't push the decade toward `Done`.
pub fn tree_fold(decade: &DecadeNode) -> DecadeRollup {
    let mut thread_rollups: Vec<ThreadRollup> = decade.threads.iter().map(fold_thread).collect();
    // Sort by thread_id so the output is deterministic regardless of
    // input ordering — invariant 13 demands this.
    thread_rollups.sort_by(|a, b| a.thread_id.cmp(&b.thread_id));

    let total = thread_rollups.len();
    let done = thread_rollups
        .iter()
        .filter(|t| t.status == RollupStatus::Done)
        .count();

    let status = if total == 0 {
        RollupStatus::Empty
    } else if thread_rollups
        .iter()
        .any(|t| t.status == RollupStatus::Blocked)
    {
        RollupStatus::Blocked
    } else if done == total {
        RollupStatus::Done
    } else {
        RollupStatus::InProgress
    };

    DecadeRollup {
        decade_id: decade.decade_id.clone(),
        threads: thread_rollups,
        threads_done: done,
        threads_total: total,
        status,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn bead(id: &str, status: LeafStatus) -> BeadNode {
        BeadNode {
            bead_id: id.to_string(),
            status,
        }
    }

    fn thread(id: &str, beads: Vec<BeadNode>) -> ThreadNode {
        ThreadNode {
            thread_id: id.to_string(),
            beads,
        }
    }

    fn decade(id: &str, threads: Vec<ThreadNode>) -> DecadeNode {
        DecadeNode {
            decade_id: id.to_string(),
            threads,
        }
    }

    /// ADR-0010 invariant 13: tree_fold_deterministic.
    #[test]
    fn tree_fold_deterministic() {
        let d1 = decade(
            "d1",
            vec![
                thread(
                    "t-a",
                    vec![bead("b1", LeafStatus::Done), bead("b2", LeafStatus::Done)],
                ),
                thread("t-b", vec![bead("b3", LeafStatus::Open)]),
            ],
        );

        // Reorder threads at the input — output should be identical.
        let d2 = decade(
            "d1",
            vec![
                thread("t-b", vec![bead("b3", LeafStatus::Open)]),
                thread(
                    "t-a",
                    vec![bead("b2", LeafStatus::Done), bead("b1", LeafStatus::Done)],
                ),
            ],
        );

        assert_eq!(tree_fold(&d1), tree_fold(&d2));
    }

    #[test]
    fn empty_thread_is_empty() {
        let t = thread("empty", vec![]);
        let r = fold_thread(&t);
        assert_eq!(r.status, RollupStatus::Empty);
        assert_eq!(r.beads_total, 0);
    }

    #[test]
    fn all_done_thread_is_done() {
        let t = thread(
            "t",
            vec![bead("b1", LeafStatus::Done), bead("b2", LeafStatus::Done)],
        );
        let r = fold_thread(&t);
        assert_eq!(r.status, RollupStatus::Done);
        assert_eq!(r.beads_done, 2);
    }

    #[test]
    fn blocked_bead_blocks_thread() {
        let t = thread(
            "t",
            vec![
                bead("b1", LeafStatus::Done),
                bead("b2", LeafStatus::Blocked),
            ],
        );
        let r = fold_thread(&t);
        assert_eq!(r.status, RollupStatus::Blocked);
    }

    #[test]
    fn mixed_thread_is_in_progress() {
        let t = thread(
            "t",
            vec![bead("b1", LeafStatus::Done), bead("b2", LeafStatus::Open)],
        );
        let r = fold_thread(&t);
        assert_eq!(r.status, RollupStatus::InProgress);
        assert_eq!(r.beads_done, 1);
        assert_eq!(r.beads_total, 2);
    }

    #[test]
    fn decade_blocked_if_any_thread_blocked() {
        let d = decade(
            "d",
            vec![
                thread("ok", vec![bead("b1", LeafStatus::Done)]),
                thread("bad", vec![bead("b2", LeafStatus::Blocked)]),
            ],
        );
        let r = tree_fold(&d);
        assert_eq!(r.status, RollupStatus::Blocked);
    }

    #[test]
    fn decade_done_when_all_threads_done() {
        let d = decade(
            "d",
            vec![
                thread("t1", vec![bead("b1", LeafStatus::Done)]),
                thread("t2", vec![bead("b2", LeafStatus::Done)]),
            ],
        );
        let r = tree_fold(&d);
        assert_eq!(r.status, RollupStatus::Done);
        assert_eq!(r.threads_done, 2);
    }

    #[test]
    fn empty_decade_is_empty() {
        let d = decade("d", vec![]);
        let r = tree_fold(&d);
        assert_eq!(r.status, RollupStatus::Empty);
    }

    #[test]
    fn thread_order_in_output_is_sorted() {
        // Inputs in non-alpha order; output threads sorted by thread_id.
        let d = decade(
            "d",
            vec![
                thread("zeta", vec![bead("b1", LeafStatus::Done)]),
                thread("alpha", vec![bead("b2", LeafStatus::Done)]),
                thread("mu", vec![bead("b3", LeafStatus::Done)]),
            ],
        );
        let r = tree_fold(&d);
        let ids: Vec<&str> = r.threads.iter().map(|t| t.thread_id.as_str()).collect();
        assert_eq!(ids, vec!["alpha", "mu", "zeta"]);
    }
}
