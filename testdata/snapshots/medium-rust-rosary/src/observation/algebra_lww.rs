//! Last-Write-Wins (LWW) register algebra.
//!
//! Bead `obs-algebra-lww` (rosary-979603). ADR-0010 invariants 4-5.
//!
//! Used for single-valued fields where the most recent observation
//! wins: `Assignee`, `PrUrl`, `MergeSha`, `Deadline`, `Ahead`, `Behind`.
//! Tiebreak when two observations share a timestamp: lexicographic on
//! `source.as_str()` — guarantees a total order so the fold is
//! deterministic regardless of slice order.
//!
//! `pr_url=None` requires an explicit observation (carrying
//! `FieldValue::OptString(None)`) — never inferred from the absence of
//! observations. The fold's empty-set return is the field's natural
//! "no observations yet" value, which for nullable fields is
//! `OptString(None)` and for required fields is the algebra-specific
//! identity below.

use anyhow::{Result, anyhow};

use super::{FieldAlgebra, FieldName, FieldValue, Observation};

/// LWW-register over a specific [`FieldName`].
///
/// One algebra instance per field — the value-type expectation differs
/// per field (`Assignee` is `OptString`, `Ahead` is `Int64`, etc.) and
/// the algebra rejects type-mismatched values via `Err`. The registry
/// owns one `LwwRegisterAlgebra` per LWW field.
#[derive(Debug, Clone)]
pub struct LwwRegisterAlgebra {
    field: FieldName,
}

impl LwwRegisterAlgebra {
    pub fn new(field: FieldName) -> Self {
        Self { field }
    }

    /// The "no observations yet" value for this field. Nullable fields
    /// (`Assignee`, `PrUrl`, `MergeSha`) return `OptString(None)` —
    /// callers must not infer non-presence as an explicit unset; only
    /// an explicit observation can do that (ADR-0010 invariant 5).
    /// Numeric fields return `Int64(0)`. Timestamp returns the epoch.
    fn empty(&self) -> FieldValue {
        match self.field {
            FieldName::Assignee
            | FieldName::PrUrl
            | FieldName::MergeSha
            | FieldName::Comment
            | FieldName::Label
            | FieldName::Status => FieldValue::OptString(None),
            FieldName::Ahead | FieldName::Behind => FieldValue::Int64(0),
            FieldName::Deadline => FieldValue::Timestamp(chrono::DateTime::UNIX_EPOCH),
            FieldName::PipelineVerdict => {
                // PipelineVerdict has its own algebra (chain-max), but
                // we still need a sensible empty for the trait.
                FieldValue::PipelineVerdict(super::PipelineVerdictValue::Dispatched)
            }
            FieldName::Other(_) => FieldValue::OptString(None),
        }
    }

    fn type_check(&self, v: &FieldValue) -> Result<()> {
        let ok = match self.field {
            FieldName::Assignee
            | FieldName::PrUrl
            | FieldName::MergeSha
            | FieldName::Comment
            | FieldName::Label
            | FieldName::Status => matches!(v, FieldValue::OptString(_) | FieldValue::String(_)),
            FieldName::Ahead | FieldName::Behind => matches!(v, FieldValue::Int64(_)),
            FieldName::Deadline => matches!(v, FieldValue::Timestamp(_)),
            FieldName::PipelineVerdict => matches!(v, FieldValue::PipelineVerdict(_)),
            FieldName::Other(_) => true, // plugin-defined; trust the caller
        };
        if ok {
            Ok(())
        } else {
            Err(anyhow!(
                "LwwRegisterAlgebra({:?}): type mismatch — got {:?}",
                self.field,
                v
            ))
        }
    }

    /// Normalize raw `String` to `OptString(Some(_))` so callers passing
    /// either form for an `Assignee`-style field get consistent results.
    fn normalize(&self, v: FieldValue) -> FieldValue {
        match (self.field.clone(), v) {
            (
                FieldName::Assignee
                | FieldName::PrUrl
                | FieldName::MergeSha
                | FieldName::Comment
                | FieldName::Label
                | FieldName::Status,
                FieldValue::String(s),
            ) => FieldValue::OptString(Some(s)),
            (_, other) => other,
        }
    }
}

impl FieldAlgebra for LwwRegisterAlgebra {
    fn field_name(&self) -> FieldName {
        self.field.clone()
    }

    fn fold(&self, obs: &[&Observation]) -> Result<FieldValue> {
        // Find the LWW winner: max by (observed_at, source.as_str()).
        // The pair gives a total order so the fold is deterministic
        // regardless of input order (ADR-0010 invariant 9). On exact
        // tie of both, slice index doesn't matter — we keep the first.
        let mut winner: Option<&&Observation> = None;
        for o in obs {
            self.type_check(&o.value)?;
            winner = match winner {
                None => Some(o),
                Some(w) => {
                    let cmp =
                        (o.observed_at, o.source.as_str()).cmp(&(w.observed_at, w.source.as_str()));
                    if cmp.is_gt() { Some(o) } else { Some(w) }
                }
            };
        }
        match winner {
            None => Ok(self.empty()),
            Some(o) => Ok(self.normalize(o.value.clone())),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{Source, WorkRef};
    use chrono::{DateTime, Utc};

    fn at(secs: i64) -> DateTime<Utc> {
        DateTime::from_timestamp(secs, 0).unwrap()
    }

    fn obs_assignee(value: &str, source: &str, observed_at: DateTime<Utc>) -> Observation {
        Observation {
            work_item: WorkRef {
                repo: "rosary".to_string(),
                scope: String::new(),
                bead_id: "rosary-test".to_string(),
            },
            source: Source::new(source),
            source_event_id: format!("e-{source}"),
            field: FieldName::Assignee,
            value: FieldValue::OptString(Some(value.to_string())),
            observed_at,
            cert: None,
            payload_hash: format!("{source}-{value}"),
        }
    }

    /// ADR-0010 invariant 4: lww_tiebreak_total. Equal `observed_at` →
    /// resolved by `source.as_str()` lex.
    #[test]
    fn lww_tiebreak_total() {
        let alg = LwwRegisterAlgebra::new(FieldName::Assignee);
        let same_ts = at(1000);
        // "github" > "bead" lex; same timestamp → github wins.
        let a = obs_assignee("alice", "bead", same_ts);
        let b = obs_assignee("bob", "github", same_ts);
        let r1 = alg.fold(&[&a, &b]).unwrap();
        let r2 = alg.fold(&[&b, &a]).unwrap();
        assert_eq!(r1, r2, "tiebreak must be order-independent");
        assert_eq!(r1, FieldValue::OptString(Some("bob".to_string())));
    }

    /// ADR-0010 invariant 5: lww_unset_explicit. `pr_url=None` requires
    /// an explicit observation; never inferred from absence.
    #[test]
    fn lww_unset_explicit() {
        let alg = LwwRegisterAlgebra::new(FieldName::PrUrl);
        // No observations → fold returns the algebra's "empty" value
        // (OptString(None)), which is by-construction not "the latest
        // observation said None" — it's "no observation yet". The
        // distinction matters at the derivation layer; here we just
        // verify the empty case doesn't claim a winner.
        let r = alg.fold(&[]).unwrap();
        assert_eq!(r, FieldValue::OptString(None));

        // Now observe Some(url), then explicitly observe None — the
        // explicit None must win (it's later).
        let workref = WorkRef {
            repo: "r".to_string(),
            scope: String::new(),
            bead_id: "b".to_string(),
        };
        let observed = Observation {
            work_item: workref.clone(),
            source: Source::new("github"),
            source_event_id: "e1".to_string(),
            field: FieldName::PrUrl,
            value: FieldValue::OptString(Some("https://example/pr/1".to_string())),
            observed_at: at(1000),
            cert: None,
            payload_hash: "h1".to_string(),
        };
        let unset = Observation {
            work_item: workref.clone(),
            source: Source::new("github"),
            source_event_id: "e2".to_string(),
            field: FieldName::PrUrl,
            value: FieldValue::OptString(None),
            observed_at: at(2000),
            cert: None,
            payload_hash: "h2".to_string(),
        };
        let r = alg.fold(&[&observed, &unset]).unwrap();
        assert_eq!(
            r,
            FieldValue::OptString(None),
            "explicit unset at later timestamp must win",
        );
    }

    #[test]
    fn lww_picks_latest() {
        let alg = LwwRegisterAlgebra::new(FieldName::Assignee);
        let a = obs_assignee("alice", "src1", at(1000));
        let b = obs_assignee("bob", "src1", at(2000));
        let c = obs_assignee("carol", "src1", at(1500));
        let r = alg.fold(&[&a, &b, &c]).unwrap();
        assert_eq!(r, FieldValue::OptString(Some("bob".to_string())));
    }

    #[test]
    fn lww_reorder_invariant() {
        let alg = LwwRegisterAlgebra::new(FieldName::Assignee);
        let a = obs_assignee("alice", "src1", at(1000));
        let b = obs_assignee("bob", "src2", at(2000));
        let c = obs_assignee("carol", "src3", at(1500));
        let r1 = alg.fold(&[&a, &b, &c]).unwrap();
        let r2 = alg.fold(&[&c, &a, &b]).unwrap();
        let r3 = alg.fold(&[&b, &c, &a]).unwrap();
        assert_eq!(r1, r2);
        assert_eq!(r2, r3);
    }

    #[test]
    fn lww_type_mismatch_errors() {
        let alg = LwwRegisterAlgebra::new(FieldName::Ahead);
        let bad = obs_assignee("not-a-number", "src", at(1000));
        let r = alg.fold(&[&bad]);
        assert!(r.is_err());
    }

    #[test]
    fn lww_field_name() {
        let alg = LwwRegisterAlgebra::new(FieldName::PrUrl);
        assert_eq!(alg.field_name(), FieldName::PrUrl);
    }
}
