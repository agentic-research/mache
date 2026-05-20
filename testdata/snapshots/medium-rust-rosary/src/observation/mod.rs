//! Observation lattice substrate (ADR-0010).
//!
//! N peer sources (Linear, GitHub, git, beads, future Slack/Notion/calendar)
//! emit authenticated [`Observation`]s about the same underlying work item.
//! A deterministic per-field fold produces the derived view.
//!
//! Phase 1 (this PR) lands the type surface and the [`Observer`] /
//! [`FieldAlgebra`] traits, plus inline unit tests for the substrate types
//! (serde round-trip, [`PipelineVerdictValue::rank`] ordering). The
//! 14-invariant integration contract referenced by ADR-0010 lives in
//! `tests/observation_lattice.rs` and lands incrementally with the
//! follow-up beads — each algebra/log/quarantine/registry/fold bead adds
//! the invariants it can prove against its own implementation. Per-field
//! algebras (chain-max, LWW, OR-set, flat-lattice), the in-memory
//! observation log, the quarantine path, and the registry+fold all ship in
//! their own beads against parallel-safe non-overlapping file scopes — see
//! the bead breakdown in `.claude/plans/`.
//!
//! # Trust model
//!
//! Two distinct authentication concerns are kept separate (ADR-0010 §"Two
//! trust boundaries"):
//!
//! - **Inbound** webhooks from external sources are HMAC-verified at the
//!   receiver against the configured webhook_secret. Observations land with
//!   [`Observation::cert`] = `None`. The user's signet key is not involved.
//! - **Outbound** observations authored by the user (or an agent on their
//!   behalf) carry a [`SignetCert`] in [`Observation::cert`] as an
//!   attestation of authorship — what makes federated mirroring auditable.

use std::time::Duration;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

pub use crate::store::WorkRef;

// Submodules. Each parallel bead owns exactly one of these files; placeholder
// bodies live in the files until the corresponding bead lands. They compile
// to empty modules so the substrate's public surface (this `mod.rs`) is
// reviewable on its own.
pub mod algebra_chain;
pub mod algebra_flat;
pub mod algebra_lww;
pub mod algebra_orset;
pub mod fold;
pub mod log;
pub mod log_sqlite;
pub mod quarantine;
pub mod registry;
pub mod tree_fold;

#[cfg(test)]
mod integration_tests;

// ── Identity of the source emitting an observation ───────────────────────

/// Identifier of the source emitting an observation.
///
/// Wrapped string rather than a closed enum so plugins can register new
/// sources without an enum bump. Canonical form is **lowercase, trimmed,
/// internal-whitespace replaced with `_`** (e.g. `"linear"`, `"github"`,
/// `"git"`, `"bead"`, `"user"`). [`Source::new`] and the `From<&str>` impl
/// enforce this so that `(Source, source_event_id, payload_hash)` is a
/// stable dedup key regardless of how a caller spelled the source name —
/// `"GitHub"`, `" github "`, and `"github"` all canonicalize to one
/// equivalence class. Without normalization the dedup set would split on
/// case (ADR-0010 invariant 8 would fail under reasonable caller mistakes).
///
/// The `pub String` field is preserved for ergonomic destructuring; if you
/// reach in directly, you are responsible for canonicalizing — prefer
/// [`Source::new`].
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub struct Source(pub String);

impl Source {
    /// Construct a [`Source`] in canonical form.
    ///
    /// Trims surrounding whitespace, lowercases, and replaces any internal
    /// whitespace runs with a single `_`. Empty input is preserved as-is —
    /// callers should treat empty `Source` as a programming error, but the
    /// constructor does not panic.
    pub fn new(s: impl Into<String>) -> Self {
        Self(canonicalize_source(&s.into()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl From<&str> for Source {
    fn from(s: &str) -> Self {
        Source::new(s)
    }
}

fn canonicalize_source(raw: &str) -> String {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    let lowered = trimmed.to_lowercase();
    // Collapse any internal whitespace run to a single `_`.
    let mut out = String::with_capacity(lowered.len());
    let mut last_was_ws = false;
    for ch in lowered.chars() {
        if ch.is_whitespace() {
            if !last_was_ws {
                out.push('_');
            }
            last_was_ws = true;
        } else {
            out.push(ch);
            last_was_ws = false;
        }
    }
    out
}

// ── Field identity and value ────────────────────────────────────────────

/// Logical field on a work item. Each field declares a per-field algebra
/// (chain-max, LWW-register, OR-set, flat-lattice) at registry-load time;
/// the fold dispatches by [`FieldName`].
///
/// Open-ended via [`FieldName::Other`] so plugins can register new fields.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FieldName {
    /// Agent-pipeline verdict within a bead. Chain-max algebra.
    /// (Mirrors `src/dolt/observations.rs::Verdict` in the substrate.)
    /// Renamed from `PipelinePhase` per ADR-0010 review to avoid name
    /// collision with `crate::store::PipelineState::pipeline_phase`
    /// (which is a `u8` index into the agent sequence — a different
    /// quantity entirely).
    PipelineVerdict,
    /// Single-valued, identity. LWW-register.
    Assignee,
    /// Single-valued, may change. LWW-register; `None` requires explicit unset.
    PrUrl,
    /// Single-valued, immutable in practice. LWW-register.
    MergeSha,
    /// Single-valued, time. LWW-register.
    Deadline,
    /// Numeric, replaces. LWW-register.
    Ahead,
    /// Numeric, replaces. LWW-register.
    Behind,
    /// Set-accumulating. OR-set keyed on `(Source, source_event_id)`.
    Comment,
    /// Set-accumulating. OR-set.
    Label,
    /// Derived (not primitive). Cross-source result joins via flat lattice
    /// with `⊤ = Conflict`. Sources disagreeing surfaces witnesses.
    Status,
    /// Plugin-defined.
    Other(String),
}

/// Typed sum of values an observation can carry.
///
/// Open-ended in the same sense as [`FieldName`]: [`FieldValue::Json`] is the
/// escape hatch for plugin-defined fields. Algebras are responsible for
/// rejecting type-mismatched values via [`anyhow::Error`] (which routes the
/// observation to quarantine, not a panic).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "kind", content = "value", rename_all = "snake_case")]
pub enum FieldValue {
    /// Free-form string (e.g. assignee handle, comment text, label name).
    String(String),
    /// Single-valued nullable string. The `None` case requires an explicit
    /// observation — never inferred from absence (ADR-0010 invariant 5).
    OptString(Option<String>),
    /// Numeric.
    Int64(i64),
    /// Wall-clock time.
    Timestamp(DateTime<Utc>),
    /// Pipeline-phase value (chain ordering).
    PipelineVerdict(PipelineVerdictValue),
    /// Plugin-defined fields use raw JSON; typed algebras must downcast.
    Json(serde_json::Value),
}

/// Chain-ordered value space for [`FieldName::PipelineVerdict`].
///
/// Distinct from `crate::dolt::observations::Verdict` so the substrate does
/// not depend on `dolt::*`. The chain is a real partial order: agent runs
/// within a bead are monotone — `Pass` never steps back to `Dispatched`.
///
/// `Fail` and `Deadletter` carry no rank (ADR-0010 invariant 3) — they don't
/// advance the chain.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PipelineVerdictValue {
    Dispatched,
    Verifying,
    Pass,
    PrOpen,
    Done,
    Fail,
    Deadletter,
}

impl PipelineVerdictValue {
    /// Lattice rank for chain-max. `None` = does not advance.
    pub fn rank(self) -> Option<u8> {
        match self {
            PipelineVerdictValue::Dispatched => Some(1),
            PipelineVerdictValue::Verifying => Some(2),
            PipelineVerdictValue::Pass => Some(3),
            PipelineVerdictValue::PrOpen => Some(4),
            PipelineVerdictValue::Done => Some(5),
            PipelineVerdictValue::Fail | PipelineVerdictValue::Deadletter => None,
        }
    }
}

// ── Identity attestation (signet cert) ──────────────────────────────────

/// Stub for the signet ephemeral cert attached to user-authored observations.
///
/// Phase 2 wires this through `src/dsse.rs`. Phase 1 only carries the
/// envelope shape so the [`Observation`] struct is stable; cert validation
/// in [`crate::observation::quarantine`] is a stub that always returns `Ok`
/// for Phase 1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SignetCert {
    /// Hex-encoded SHA-256 of the verifying public key.
    pub key_id: String,
    /// Base64 of the Ed25519 signature over the canonical observation
    /// payload.
    pub signature: String,
}

// ── The observation itself ──────────────────────────────────────────────

/// One authenticated observation about a single field of a single work item.
///
/// Observations are append-only and form a G-set keyed by
/// `(source, source_event_id, payload_hash)` — that is the dedup key. The
/// fold consumes this set via per-field algebras (ADR-0010).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Observation {
    pub work_item: WorkRef,
    pub source: Source,
    /// Webhook event id, poll cursor token, or manual ack — whatever uniquely
    /// identifies this observation event at the source. Combined with `source`
    /// and `payload_hash` to dedup replays.
    pub source_event_id: String,
    pub field: FieldName,
    pub value: FieldValue,
    pub observed_at: DateTime<Utc>,
    /// Identity attestation. `Some` for outbound user-authored observations;
    /// `None` for inbound HMAC-verified webhooks (the source signature was
    /// already checked at the receiver).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cert: Option<SignetCert>,
    /// BLAKE3 (or any fixed-output hash) over the canonical
    /// `(source, field, value)` triple. Combined with
    /// `(source, source_event_id)` to dedup.
    pub payload_hash: String,
}

impl Observation {
    /// The dedup key. Two observations with the same key are the same event;
    /// the second insertion is a no-op (ADR-0010 invariant 8).
    pub fn dedup_key(&self) -> DedupKey {
        DedupKey {
            source: self.source.clone(),
            source_event_id: self.source_event_id.clone(),
            payload_hash: self.payload_hash.clone(),
        }
    }
}

/// Tuple key used by [`crate::observation::log`] to dedup observation
/// inserts before they reach the fold.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub struct DedupKey {
    pub source: Source,
    pub source_event_id: String,
    pub payload_hash: String,
}

// ── Observer + cadence ──────────────────────────────────────────────────

/// How an [`Observer`] is woken to emit observations.
#[derive(Debug, Clone)]
pub enum Cadence {
    /// Observer is webhook-driven; rosary's webhook router invokes
    /// [`Observer::observe`] when a verified payload arrives.
    Webhook,
    /// Observer polls on the given interval. Subject to per-source rate
    /// budget (ADR-0010 §Rate budget) — anti-shadowban guarantee.
    Poll(Duration),
    /// Observer runs only when explicitly invoked (e.g. once per reconcile
    /// tick). No background scheduling.
    OnDemand,
}

/// Read-only context handed to an [`Observer::observe`] call.
///
/// Phase 1 stub: just carries `now()` so observers can stamp `observed_at`.
/// Phase 2+ adds rate-budget tokens, last-cursor state, and a handle to the
/// observation log for incremental diff observers.
#[derive(Debug, Clone)]
pub struct ObserveCtx {
    pub now: DateTime<Utc>,
}

/// A peer source feeding the observation lattice. Each observer's writes are
/// idempotent under the dedup key — replays are safe.
#[async_trait]
pub trait Observer: Send + Sync {
    /// Stable identifier (e.g. `"linear"`, `"github_webhook"`).
    fn id(&self) -> &str;

    fn cadence(&self) -> Cadence;

    /// Produce zero or more observations from a wake event.
    ///
    /// Returning `Ok(vec![])` is normal — the wake produced no new
    /// information. Network errors, auth failures, and parse errors return
    /// `Err`; the substrate's caller (`reconcile::observation_loop` in a
    /// future bead) is responsible for back-pressure / rate-budget decay
    /// on `Err` so a flapping source doesn't burn the budget. An `Err`
    /// from one observer never aborts other observers in the same wake.
    async fn observe(&self, ctx: &ObserveCtx) -> anyhow::Result<Vec<Observation>>;
}

// ── Per-field algebra ───────────────────────────────────────────────────

/// Object-safe algebra over a set of observations.
///
/// Each registered field provides one impl; the registry stores
/// `Box<dyn FieldAlgebra>`. The fold is **N-way** (not pairwise) because
/// non-pairwise-mergeable algebras like LWW-register need access to
/// observation metadata (timestamp, source) for tiebreak — a pairwise
/// `join(a, b)` of bare values cannot satisfy commutativity for LWW.
/// Chain-max and OR-set could be expressed pairwise but using the same
/// N-way shape across all algebras keeps the registry surface uniform.
///
/// Implementations must satisfy:
/// - **Determinism**: same observation set → same result, regardless of
///   slice order. Tested empirically by `reorder_invariance` in the
///   integration contract.
/// - **Idempotence under dedup**: duplicates filtered by
///   [`Observation::dedup_key`] do not change the result (ADR-0010
///   invariant 8).
/// - **Type tolerance**: type-mismatched observations are an `Err`
///   (caller routes to quarantine), never a panic.
///
/// See `tests/observation_lattice.rs` for the 14-invariant contract
/// (ADR-0010 §"Test contract").
pub trait FieldAlgebra: Send + Sync {
    /// Field this algebra handles. Used by the registry for routing.
    fn field_name(&self) -> FieldName;

    /// Reduce a set of observations on this field to the derived value.
    ///
    /// The empty case must produce a sensible "no observations yet" value
    /// (e.g. `FieldValue::OptString(None)` for nullable single-value
    /// fields, the lowest chain element for chain-max). Implementations
    /// MUST be invariant under reordering of `obs`.
    fn fold(&self, obs: &[&Observation]) -> anyhow::Result<FieldValue>;
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_workref() -> WorkRef {
        WorkRef {
            repo: "rosary".to_string(),
            scope: String::new(),
            bead_id: "rosary-97660c".to_string(),
        }
    }

    fn sample_observation() -> Observation {
        Observation {
            work_item: sample_workref(),
            source: Source::new("github"),
            source_event_id: "evt_42".to_string(),
            field: FieldName::PrUrl,
            value: FieldValue::OptString(Some("https://github.com/foo/bar/pull/186".to_string())),
            observed_at: DateTime::parse_from_rfc3339("2026-05-09T12:00:00Z")
                .unwrap()
                .with_timezone(&Utc),
            cert: None,
            payload_hash: "abc123".to_string(),
        }
    }

    #[test]
    fn observation_serde_roundtrip() {
        let obs = sample_observation();
        let json = serde_json::to_string(&obs).unwrap();
        let back: Observation = serde_json::from_str(&json).unwrap();
        assert_eq!(obs, back);
    }

    #[test]
    fn observation_with_cert_roundtrip() {
        let mut obs = sample_observation();
        obs.cert = Some(SignetCert {
            key_id: "abcd".to_string(),
            signature: "ZGVhZGJlZWY=".to_string(),
        });
        let json = serde_json::to_string(&obs).unwrap();
        let back: Observation = serde_json::from_str(&json).unwrap();
        assert_eq!(obs, back);
    }

    #[test]
    fn dedup_key_components() {
        let obs = sample_observation();
        let key = obs.dedup_key();
        assert_eq!(key.source, Source::new("github"));
        assert_eq!(key.source_event_id, "evt_42");
        assert_eq!(key.payload_hash, "abc123");
    }

    #[test]
    fn dedup_key_equality_under_clone() {
        let obs = sample_observation();
        assert_eq!(obs.dedup_key(), obs.clone().dedup_key());
    }

    #[test]
    fn field_name_serde_snake_case() {
        let name = FieldName::PipelineVerdict;
        let json = serde_json::to_string(&name).unwrap();
        assert_eq!(json, "\"pipeline_verdict\"");
        let back: FieldName = serde_json::from_str(&json).unwrap();
        assert_eq!(name, back);
    }

    #[test]
    fn field_name_other_roundtrip() {
        let name = FieldName::Other("plugin_field".to_string());
        let json = serde_json::to_string(&name).unwrap();
        let back: FieldName = serde_json::from_str(&json).unwrap();
        assert_eq!(name, back);
    }

    #[test]
    fn field_value_variants_roundtrip() {
        let cases = vec![
            FieldValue::String("hi".to_string()),
            FieldValue::OptString(Some("x".to_string())),
            FieldValue::OptString(None),
            FieldValue::Int64(-3),
            FieldValue::Timestamp(
                DateTime::parse_from_rfc3339("2026-05-09T12:00:00Z")
                    .unwrap()
                    .with_timezone(&Utc),
            ),
            FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
            FieldValue::Json(serde_json::json!({ "extra": 1 })),
        ];
        for v in cases {
            let json = serde_json::to_string(&v).unwrap();
            let back: FieldValue = serde_json::from_str(&json).unwrap();
            assert_eq!(v, back);
        }
    }

    #[test]
    fn pipeline_phase_chain_rank() {
        // Chain ranks are strictly increasing for the in-chain variants.
        assert!(PipelineVerdictValue::Dispatched.rank() < PipelineVerdictValue::Verifying.rank());
        assert!(PipelineVerdictValue::Verifying.rank() < PipelineVerdictValue::Pass.rank());
        assert!(PipelineVerdictValue::Pass.rank() < PipelineVerdictValue::PrOpen.rank());
        assert!(PipelineVerdictValue::PrOpen.rank() < PipelineVerdictValue::Done.rank());
    }

    #[test]
    fn pipeline_phase_unranked_variants_have_no_rank() {
        assert!(PipelineVerdictValue::Fail.rank().is_none());
        assert!(PipelineVerdictValue::Deadletter.rank().is_none());
    }

    #[test]
    fn source_helpers() {
        let s = Source::new("linear");
        assert_eq!(s.as_str(), "linear");
        assert_eq!(s, Source::from("linear"));
    }

    #[test]
    fn source_canonicalizes_case_and_whitespace() {
        // Canonical form: lowercase, trimmed, internal whitespace → `_`.
        // Without this, `(Source, source_event_id, payload_hash)` would
        // split the dedup set on case (ADR-0010 invariant 8).
        assert_eq!(Source::new("GitHub").as_str(), "github");
        assert_eq!(Source::new(" github ").as_str(), "github");
        assert_eq!(Source::new("Slack Channel").as_str(), "slack_channel");
        assert_eq!(Source::new("  linear   poller ").as_str(), "linear_poller");
        // From<&str> goes through the same path.
        assert_eq!(Source::from("GITHUB"), Source::new("github"));
    }

    #[test]
    fn source_dedup_key_invariant_under_caller_mistakes() {
        // Two callers writing the same source name with different casing
        // must produce the same dedup key.
        let a = sample_observation();
        let mut b = sample_observation();
        b.source = Source::new("GitHub"); // same logical source, ugly spelling
        assert_eq!(a.dedup_key(), b.dedup_key());
    }

    #[test]
    fn signet_cert_roundtrip() {
        let cert = SignetCert {
            key_id: "abc".to_string(),
            signature: "sig".to_string(),
        };
        let json = serde_json::to_string(&cert).unwrap();
        let back: SignetCert = serde_json::from_str(&json).unwrap();
        assert_eq!(cert, back);
    }

    #[test]
    fn cadence_variants_construct() {
        // Cadence is non-serde (carries Duration); just exercise the
        // constructor surface so a future refactor that drops a variant
        // breaks the test.
        let _ = Cadence::Webhook;
        let _ = Cadence::Poll(Duration::from_secs(60));
        let _ = Cadence::OnDemand;
    }

    #[test]
    fn workref_reexport_constructs() {
        // The substrate re-exports WorkRef from src/store.rs:23. This test
        // pins that re-export so renaming the upstream type breaks here.
        let _: WorkRef = sample_workref();
    }
}
