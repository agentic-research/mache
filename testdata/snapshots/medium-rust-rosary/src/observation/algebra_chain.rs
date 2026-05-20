//! Chain-max algebra for [`crate::observation::FieldName::PipelineVerdict`].
//!
//! Bead `obs-algebra-chain` (rosary-9780c8). ADR-0010 invariants 1-3.
//!
//! Agent-pipeline progress within a bead is genuinely monotone:
//! `Dispatched < Verifying < Pass < PrOpen < Done`. No agent ever steps
//! a bead from `Pass` back to `Dispatched`, so the chain ordering is a
//! real partial order (unlike the workflow ordering for user-facing
//! `status`, which has back-edges and is not a poset). `Fail` and
//! `Deadletter` carry no rank — they don't advance the chain.
//!
//! This algebra mirrors the by-hand implementation in
//! `src/dolt/observations.rs::Verdict` (rosary-45518d), now generalized
//! as one registered field in the substrate.

use anyhow::{Result, anyhow};

use super::{FieldAlgebra, FieldName, FieldValue, Observation, PipelineVerdictValue};

/// Chain-max algebra over [`PipelineVerdictValue`].
///
/// Stateless — instantiate once and register. The fold returns the
/// rank-maximum value across all observations; ties keep the first
/// rank-tied value seen; observations of unranked variants (`Fail`,
/// `Deadletter`) are skipped. An empty observation set returns the
/// chain's minimum (`Dispatched`).
#[derive(Debug, Default, Clone, Copy)]
pub struct ChainMaxAlgebra;

impl FieldAlgebra for ChainMaxAlgebra {
    fn field_name(&self) -> FieldName {
        FieldName::PipelineVerdict
    }

    fn fold(&self, obs: &[&Observation]) -> Result<FieldValue> {
        let mut best: Option<PipelineVerdictValue> = None;
        let mut best_rank: Option<u8> = None;
        for o in obs {
            let v = match &o.value {
                FieldValue::PipelineVerdict(p) => *p,
                other => {
                    return Err(anyhow!(
                        "ChainMaxAlgebra: expected FieldValue::PipelineVerdict, got {other:?} \
                         (source={}, source_event_id={})",
                        o.source.as_str(),
                        o.source_event_id
                    ));
                }
            };
            let r = match v.rank() {
                Some(r) => r,
                // Unranked variants (Fail, Deadletter) are transparent —
                // they don't advance the chain (ADR-0010 invariant 3).
                None => continue,
            };
            match best_rank {
                Some(br) if r <= br => {}
                _ => {
                    best = Some(v);
                    best_rank = Some(r);
                }
            }
        }
        Ok(FieldValue::PipelineVerdict(
            best.unwrap_or(PipelineVerdictValue::Dispatched),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{Source, WorkRef};
    use chrono::Utc;

    fn obs(verdict: PipelineVerdictValue, source: &str, evt: &str) -> Observation {
        Observation {
            work_item: WorkRef {
                repo: "rosary".to_string(),
                scope: String::new(),
                bead_id: "rosary-test".to_string(),
            },
            source: Source::new(source),
            source_event_id: evt.to_string(),
            field: FieldName::PipelineVerdict,
            value: FieldValue::PipelineVerdict(verdict),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: format!("{evt}-{verdict:?}"),
        }
    }

    fn fold_verdicts(verdicts: &[PipelineVerdictValue]) -> PipelineVerdictValue {
        let observations: Vec<Observation> = verdicts
            .iter()
            .enumerate()
            .map(|(i, v)| obs(*v, "test", &format!("e{i}")))
            .collect();
        let refs: Vec<&Observation> = observations.iter().collect();
        match ChainMaxAlgebra.fold(&refs).unwrap() {
            FieldValue::PipelineVerdict(p) => p,
            _ => panic!("non-PipelineVerdict result"),
        }
    }

    /// ADR-0010 invariant 1: chain_max_idempotent.
    #[test]
    fn chain_max_idempotent() {
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Pass, PipelineVerdictValue::Pass]),
            PipelineVerdictValue::Pass,
        );
    }

    /// ADR-0010 invariant 2: chain_max_monotone.
    #[test]
    fn chain_max_monotone() {
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Dispatched, PipelineVerdictValue::Pass]),
            PipelineVerdictValue::Pass,
        );
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Pass, PipelineVerdictValue::Dispatched]),
            PipelineVerdictValue::Pass,
        );
    }

    /// ADR-0010 invariant 3: chain_max_unranked_ignored.
    #[test]
    fn chain_max_unranked_ignored() {
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Pass, PipelineVerdictValue::Fail]),
            PipelineVerdictValue::Pass,
        );
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Pass, PipelineVerdictValue::Deadletter]),
            PipelineVerdictValue::Pass,
        );
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Dispatched, PipelineVerdictValue::Fail]),
            PipelineVerdictValue::Dispatched,
        );
    }

    #[test]
    fn chain_max_associative_under_reorder() {
        let cases: Vec<Vec<PipelineVerdictValue>> = vec![
            vec![
                PipelineVerdictValue::Pass,
                PipelineVerdictValue::Dispatched,
                PipelineVerdictValue::Verifying,
            ],
            vec![
                PipelineVerdictValue::Verifying,
                PipelineVerdictValue::Pass,
                PipelineVerdictValue::Dispatched,
            ],
            vec![
                PipelineVerdictValue::Dispatched,
                PipelineVerdictValue::Verifying,
                PipelineVerdictValue::Pass,
            ],
        ];
        let golden = fold_verdicts(&cases[0]);
        for c in &cases[1..] {
            assert_eq!(fold_verdicts(c), golden);
        }
    }

    #[test]
    fn chain_max_empty_returns_dispatched() {
        assert_eq!(fold_verdicts(&[]), PipelineVerdictValue::Dispatched);
    }

    #[test]
    fn chain_max_all_unranked_returns_dispatched() {
        assert_eq!(
            fold_verdicts(&[PipelineVerdictValue::Fail, PipelineVerdictValue::Deadletter]),
            PipelineVerdictValue::Dispatched,
        );
    }

    #[test]
    fn chain_max_type_mismatch_errors() {
        let bad = Observation {
            work_item: WorkRef {
                repo: "rosary".to_string(),
                scope: String::new(),
                bead_id: "rosary-test".to_string(),
            },
            source: Source::new("test"),
            source_event_id: "e0".to_string(),
            field: FieldName::PipelineVerdict,
            value: FieldValue::String("not a verdict".to_string()),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: "x".to_string(),
        };
        let r = ChainMaxAlgebra.fold(&[&bad]);
        assert!(r.is_err());
        assert!(r.unwrap_err().to_string().contains("ChainMaxAlgebra"));
    }

    #[test]
    fn chain_max_field_name() {
        assert_eq!(ChainMaxAlgebra.field_name(), FieldName::PipelineVerdict);
    }
}
