//! Cert-validity filter + queryable quarantine surface.
//!
//! Bead `obs-storage-and-quarantine` (rosary-97e386). ADR-0010
//! invariants 11-12 (`quarantine_does_not_join`, `quarantine_is_queryable`).
//!
//! Phase 1 cert validation is a stub: any observation with `cert: Some(_)`
//! that "fails" (per the stub's always-`Ok` policy) is quarantined; all
//! others pass. Phase 2 wires `crate::dsse` to actually verify the
//! Ed25519 signature. The substrate's contract — quarantined observations
//! are NOT joined into the derived view, AND they're queryable so they
//! don't disappear silently — is structural and ready for the real cert
//! check to drop in.
//!
//! Inbound webhook observations have `cert: None` (the source's HMAC
//! was verified at the receiver). Those pass through; `cert: None` is
//! NOT a quarantine signal — it's the expected state for them. See
//! ADR-0010 §"Two trust boundaries".

use serde::{Deserialize, Serialize};

use super::Observation;

/// Why an observation is in quarantine. Open enum so future failure
/// modes (revoked cert, expired cert, replay-attack-window violation,
/// rate-budget exhaustion) extend cleanly.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum QuarantineReason {
    /// Cert was supplied but failed signature verification.
    InvalidCert { detail: String },
    /// Plugin-defined reason — open variant for forward extensibility.
    /// `name` (not `kind`) avoids colliding with the enum's serde tag.
    Other { name: String, detail: String },
}

/// One quarantined observation. Carries the original observation
/// verbatim (no truncation) plus the reason it was rejected — so audit
/// reads can reconstruct exactly what was attempted.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct QuarantineEntry {
    pub observation: Observation,
    pub reason: QuarantineReason,
    pub quarantined_at: chrono::DateTime<chrono::Utc>,
}

/// In-memory quarantine log. Sibling to `ObservationLog`; populated
/// at ingest time (before the fold) when an observation fails the
/// cert-validity check.
#[derive(Debug, Default, Clone)]
pub struct QuarantineLog {
    entries: Vec<QuarantineEntry>,
}

impl QuarantineLog {
    pub fn new() -> Self {
        Self::default()
    }

    /// Record a quarantined observation. Always succeeds — quarantine
    /// is the safety valve, never the bottleneck.
    pub fn add(&mut self, obs: Observation, reason: QuarantineReason) {
        self.entries.push(QuarantineEntry {
            observation: obs,
            reason,
            quarantined_at: chrono::Utc::now(),
        });
    }

    /// Iterate all quarantined entries in insertion order. Used by
    /// `rsry status --quarantine` (Phase 2 surface) to list them.
    pub fn iter_quarantined(&self) -> impl Iterator<Item = &QuarantineEntry> {
        self.entries.iter()
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
}

/// Validate an observation's cert, if any. Phase 1 stub: returns
/// `Ok(())` for `cert: None` (expected for inbound webhooks per
/// ADR-0010 §"Two trust boundaries"), and `Ok(())` for `cert: Some(_)`
/// (Phase 2 will plug `crate::dsse::verify_envelope` here). The
/// signature is `Result` so Phase 2's signature check propagates
/// cleanly without a trait change.
///
/// To exercise the quarantine path in tests, callers construct the
/// `QuarantineReason` directly (the test fixture in this module + the
/// 14-invariant integration suite both do).
pub fn validate_cert(obs: &Observation) -> Result<(), QuarantineReason> {
    let _ = obs;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{FieldName, FieldValue, Source, WorkRef};
    use chrono::Utc;

    fn obs() -> Observation {
        Observation {
            work_item: WorkRef {
                repo: "r".to_string(),
                scope: String::new(),
                bead_id: "b".to_string(),
            },
            source: Source::new("user"),
            source_event_id: "evt-1".to_string(),
            field: FieldName::Assignee,
            value: FieldValue::OptString(Some("alice".to_string())),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: "h1".to_string(),
        }
    }

    /// ADR-0010 invariant 12: quarantine_is_queryable. Quarantined
    /// observations are surfaced via a dedicated path, not silently
    /// dropped.
    #[test]
    fn quarantine_is_queryable() {
        let mut q = QuarantineLog::new();
        q.add(
            obs(),
            QuarantineReason::InvalidCert {
                detail: "signature mismatch".to_string(),
            },
        );
        assert_eq!(q.len(), 1);
        let entries: Vec<&QuarantineEntry> = q.iter_quarantined().collect();
        assert_eq!(entries.len(), 1);
        assert!(matches!(
            entries[0].reason,
            QuarantineReason::InvalidCert { .. }
        ));
    }

    /// ADR-0010 invariant 11 lives in the integration test — verifying
    /// that quarantined observations don't appear in the derived view
    /// requires the fold (Bead 3). Here we just confirm the quarantine
    /// log itself doesn't expose its entries through any "join into
    /// derived view" path.
    #[test]
    fn quarantine_has_no_join_path_into_derived_view() {
        // Compile-time guarantee: this module exposes only `add` and
        // `iter_quarantined`. There's no `as_observation_log` or
        // similar that would let the fold see quarantined entries.
        // If you're adding a method here, ask yourself: should the
        // fold ever consume quarantined entries? Answer should always
        // be NO (per ADR-0010).
        let q = QuarantineLog::new();
        let _it = q.iter_quarantined(); // only legitimate exposure
    }

    #[test]
    fn validate_cert_stub_passes_no_cert() {
        let o = obs();
        assert!(validate_cert(&o).is_ok());
    }

    #[test]
    fn validate_cert_stub_passes_with_cert() {
        let mut o = obs();
        o.cert = Some(crate::observation::SignetCert {
            key_id: "k".to_string(),
            signature: "s".to_string(),
        });
        // Phase 1 stub doesn't actually verify; Phase 2 wires it.
        assert!(validate_cert(&o).is_ok());
    }

    #[test]
    fn quarantine_reason_serde_roundtrip() {
        let r = QuarantineReason::InvalidCert {
            detail: "expired".to_string(),
        };
        let json = serde_json::to_string(&r).unwrap();
        let back: QuarantineReason = serde_json::from_str(&json).unwrap();
        assert_eq!(r, back);
    }
}
