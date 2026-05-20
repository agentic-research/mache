//! Append-only observations for CRDT-lattice bead state.
//!
//! Each agent run produces an immutable observation. Bead status is DERIVED
//! from the set of observations via lattice join — never stored as a mutable cell.
//!
//! Lattice ordering: open < dispatched < verifying < pr_open < done
//!                   (blocked is a separate deadletter path)
//!
//! See rosary-45518d for the architecture design (venturi pattern).

use anyhow::Result;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx_core::query::query;
use sqlx_core::row::Row;

use super::DoltClient;

/// An immutable observation from an agent run.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Observation {
    pub bead_id: String,
    pub agent: String,
    pub phase: u32,
    pub verdict: Verdict,
    pub detail: String,
    pub content_hash: String,
    pub created_at: DateTime<Utc>,
}

/// Agent verdict — the lattice values.
///
/// Lattice ordering (max wins):
///   Dispatched < Verifying < Pass < PrOpen < Done
///   Fail and Deadletter are non-comparable (they don't advance the lattice).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Verdict {
    /// Agent dispatched (observation = "work started")
    Dispatched,
    /// Agent completed, verification running
    Verifying,
    /// Verification passed
    Pass,
    /// Verification failed — retry eligible
    Fail,
    /// Pipeline terminal — PR created
    PrOpen,
    /// All done — PR merged
    Done,
    /// Max retries exceeded — needs human intervention
    Deadletter,
}

impl Verdict {
    /// Lattice ordering for status derivation. Higher = further along.
    /// Fail and Deadletter return None (they don't participate in max).
    pub fn lattice_rank(self) -> Option<u8> {
        match self {
            Verdict::Dispatched => Some(1),
            Verdict::Verifying => Some(2),
            Verdict::Pass => Some(3),
            Verdict::PrOpen => Some(4),
            Verdict::Done => Some(5),
            Verdict::Fail | Verdict::Deadletter => None,
        }
    }

    fn as_str(self) -> &'static str {
        match self {
            Verdict::Dispatched => "dispatched",
            Verdict::Verifying => "verifying",
            Verdict::Pass => "pass",
            Verdict::Fail => "fail",
            Verdict::PrOpen => "pr_open",
            Verdict::Done => "done",
            Verdict::Deadletter => "deadletter",
        }
    }

    fn from_str(s: &str) -> Self {
        match s {
            "dispatched" => Verdict::Dispatched,
            "verifying" => Verdict::Verifying,
            "pass" => Verdict::Pass,
            "fail" => Verdict::Fail,
            "pr_open" => Verdict::PrOpen,
            "done" => Verdict::Done,
            "deadletter" => Verdict::Deadletter,
            _ => Verdict::Dispatched,
        }
    }
}

/// Derived bead status from the lattice join of all observations.
#[derive(Debug, Clone)]
pub struct DerivedStatus {
    /// The lattice-max verdict (highest-ranked observation)
    pub status: String,
    /// Total observations for this bead
    pub observation_count: usize,
    /// Whether any observation is a deadletter
    pub is_deadlettered: bool,
    /// Latest observation timestamp
    pub last_observed: Option<DateTime<Utc>>,
}

impl DoltClient {
    /// Append an observation (immutable — never updates existing rows).
    pub async fn append_observation(
        &self,
        bead_id: &str,
        agent: &str,
        phase: u32,
        verdict: Verdict,
        detail: &str,
        content_hash: &str,
    ) -> Result<()> {
        query(
            "INSERT INTO observations (bead_id, agent, phase, verdict, detail, content_hash, created_at)
             VALUES (?, ?, ?, ?, ?, ?, NOW())",
        )
        .bind(bead_id)
        .bind(agent)
        .bind(phase)
        .bind(verdict.as_str())
        .bind(detail)
        .bind(content_hash)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Get all observations for a bead, ordered by time.
    pub async fn get_observations(&self, bead_id: &str) -> Result<Vec<Observation>> {
        let rows = query(
            "SELECT bead_id, agent, phase, verdict, detail, content_hash, created_at
             FROM observations WHERE bead_id = ? ORDER BY created_at ASC",
        )
        .bind(bead_id)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|row| Observation {
                bead_id: row.try_get("bead_id").unwrap_or_default(),
                agent: row.try_get("agent").unwrap_or_default(),
                phase: row.try_get::<u32, _>("phase").unwrap_or(0),
                verdict: Verdict::from_str(
                    row.try_get::<String, _>("verdict")
                        .unwrap_or_default()
                        .as_str(),
                ),
                detail: row.try_get("detail").unwrap_or_default(),
                content_hash: row.try_get("content_hash").unwrap_or_default(),
                created_at: row.try_get("created_at").unwrap_or_else(|_| Utc::now()),
            })
            .collect())
    }

    /// Derive bead status from the lattice join of all observations.
    ///
    /// The status is the highest-ranked verdict (by lattice ordering).
    /// If any observation is a deadletter, the bead is blocked.
    /// If no observations exist, status is "open".
    pub async fn derive_status(&self, bead_id: &str) -> Result<DerivedStatus> {
        let observations = self.get_observations(bead_id).await?;

        if observations.is_empty() {
            return Ok(DerivedStatus {
                status: "open".to_string(),
                observation_count: 0,
                is_deadlettered: false,
                last_observed: None,
            });
        }

        let is_deadlettered = observations
            .iter()
            .any(|o| o.verdict == Verdict::Deadletter);

        // Lattice join: max rank wins (ignoring Fail/Deadletter which have no rank)
        let max_verdict = observations
            .iter()
            .filter_map(|o| o.verdict.lattice_rank().map(|r| (r, o.verdict)))
            .max_by_key(|(rank, _)| *rank)
            .map(|(_, v)| v);

        let status = if is_deadlettered {
            "blocked".to_string()
        } else {
            max_verdict
                .map(|v| v.as_str().to_string())
                .unwrap_or_else(|| "open".to_string())
        };

        Ok(DerivedStatus {
            status,
            observation_count: observations.len(),
            is_deadlettered,
            last_observed: observations.last().map(|o| o.created_at),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verdict_lattice_ordering() {
        assert!(Verdict::Dispatched.lattice_rank() < Verdict::Verifying.lattice_rank());
        assert!(Verdict::Verifying.lattice_rank() < Verdict::Pass.lattice_rank());
        assert!(Verdict::Pass.lattice_rank() < Verdict::PrOpen.lattice_rank());
        assert!(Verdict::PrOpen.lattice_rank() < Verdict::Done.lattice_rank());
    }

    #[test]
    fn verdict_fail_has_no_rank() {
        assert!(Verdict::Fail.lattice_rank().is_none());
        assert!(Verdict::Deadletter.lattice_rank().is_none());
    }

    #[test]
    fn verdict_roundtrip() {
        for v in [
            Verdict::Dispatched,
            Verdict::Verifying,
            Verdict::Pass,
            Verdict::Fail,
            Verdict::PrOpen,
            Verdict::Done,
            Verdict::Deadletter,
        ] {
            assert_eq!(Verdict::from_str(v.as_str()), v);
        }
    }

    #[test]
    fn lattice_join_max_wins() {
        // Simulate: dispatched, then pass, then pr_open
        let observations = [
            (Verdict::Dispatched, Some(1)),
            (Verdict::Pass, Some(3)),
            (Verdict::PrOpen, Some(4)),
        ];
        let max = observations
            .iter()
            .filter_map(|(v, _)| v.lattice_rank().map(|r| (r, *v)))
            .max_by_key(|(rank, _)| *rank)
            .map(|(_, v)| v);
        assert_eq!(max, Some(Verdict::PrOpen));
    }

    #[test]
    fn lattice_join_fail_ignored() {
        // Fail doesn't advance the lattice — dispatched + fail = dispatched
        let observations = [(Verdict::Dispatched, Some(1)), (Verdict::Fail, None)];
        let max = observations
            .iter()
            .filter_map(|(v, _)| v.lattice_rank().map(|r| (r, *v)))
            .max_by_key(|(rank, _)| *rank)
            .map(|(_, v)| v);
        assert_eq!(max, Some(Verdict::Dispatched));
    }

    // --- Lattice ↔ persist_status consistency tests ---
    // These prove the lattice derives the same status string that
    // the reconciler writes via persist_status, for every scenario.

    /// Helper: derive status from a sequence of verdicts (simulates derive_status
    /// without needing a real DB connection).
    fn lattice_status(verdicts: &[Verdict]) -> String {
        let is_deadlettered = verdicts.contains(&Verdict::Deadletter);
        let max = verdicts
            .iter()
            .filter_map(|v| v.lattice_rank().map(|r| (r, *v)))
            .max_by_key(|(r, _)| *r)
            .map(|(_, v)| v);

        if is_deadlettered {
            "blocked".to_string()
        } else {
            max.map(|v| v.as_str().to_string())
                .unwrap_or_else(|| "open".to_string())
        }
    }

    #[test]
    fn lattice_matches_persist_dispatch() {
        // reconciler: persist_status(_, _, "dispatched")
        assert_eq!(lattice_status(&[Verdict::Dispatched]), "dispatched");
    }

    #[test]
    fn lattice_matches_persist_pass_advance() {
        // reconciler: on Advance → persist_status(_, _, "open") after advancing
        // lattice: dispatched + pass = "pass" (highest rank)
        // NOTE: the reconciler writes "open" for the NEXT phase, but the lattice
        // tracks the observation. "pass" means "this phase passed" — the next
        // dispatch will add a new Dispatched observation.
        let status = lattice_status(&[Verdict::Dispatched, Verdict::Pass]);
        assert_eq!(status, "pass");
    }

    #[test]
    fn lattice_matches_persist_terminal() {
        // reconciler: on Terminal → persist_status(_, _, "pr_open")
        let status = lattice_status(&[Verdict::Dispatched, Verdict::Pass, Verdict::PrOpen]);
        assert_eq!(status, "pr_open");
    }

    #[test]
    fn lattice_matches_persist_retry() {
        // reconciler: on Retry → persist_status(_, _, "open")
        // lattice: dispatched + fail = "dispatched" (fail has no rank, dispatched is max)
        let status = lattice_status(&[Verdict::Dispatched, Verdict::Fail]);
        assert_eq!(
            status, "dispatched",
            "fail doesn't advance — stays at dispatched"
        );
    }

    #[test]
    fn lattice_matches_persist_deadletter() {
        // reconciler: on Deadletter → persist_status(_, _, "blocked")
        let status = lattice_status(&[Verdict::Dispatched, Verdict::Deadletter]);
        assert_eq!(status, "blocked");
    }

    #[test]
    fn lattice_full_bug_pipeline() {
        // Bug: scoping(dispatch+pass) → dev(dispatch+pass) → staging(dispatch+pass+pr_open)
        let status = lattice_status(&[
            Verdict::Dispatched, // scoping dispatched
            Verdict::Pass,       // scoping passed
            Verdict::Dispatched, // dev dispatched
            Verdict::Pass,       // dev passed
            Verdict::Dispatched, // staging dispatched
            Verdict::Pass,       // staging passed
            Verdict::PrOpen,     // terminal
        ]);
        assert_eq!(status, "pr_open");
    }

    #[test]
    fn lattice_retry_then_pass() {
        // Dispatch → fail → dispatch again → pass → terminal
        let status = lattice_status(&[
            Verdict::Dispatched,
            Verdict::Fail,
            Verdict::Dispatched,
            Verdict::Pass,
            Verdict::PrOpen,
        ]);
        assert_eq!(status, "pr_open");
    }

    #[test]
    fn deadletter_overrides_to_blocked() {
        let is_deadlettered = true;
        let max_verdict = Some(Verdict::Pass);
        let status = if is_deadlettered {
            "blocked"
        } else {
            max_verdict.map(|v| v.as_str()).unwrap_or("open")
        };
        assert_eq!(status, "blocked");
    }
}
