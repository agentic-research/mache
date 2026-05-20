//! OR-set algebra for [`crate::observation::FieldName::Comment`] and
//! [`crate::observation::FieldName::Label`].
//!
//! Bead `obs-algebra-orset` (rosary-97a010). ADR-0010 invariants 6-7.
//!
//! Each observation contributes one element to the set; identity is
//! `(source, source_event_id)` so the same payload from two sources
//! produces two distinct entries (ADR-0010 invariant 7), and replays
//! of the same `(source, source_event_id)` are deduped by the
//! observation log (invariant 8) before reaching this algebra.
//!
//! The fold returns a `FieldValue::Json` array — preserving full
//! observation provenance (source + event_id) so consumers downstream
//! can render comment threads with attribution. A simpler "set of
//! strings" output would be lossy; the registry's downstream consumers
//! (rendering / display) want to know who said what.

use anyhow::{Result, anyhow};
use serde_json::json;

use super::{FieldAlgebra, FieldName, FieldValue, Observation};

/// OR-set algebra. One instance per OR-set field (Comment, Label).
///
/// Add-only in this Phase 1 — there's no "remove" semantic yet because
/// no source emits comment-deletion observations through the substrate.
/// When that's needed (e.g. mirrored Linear comment-delete events),
/// extend with a `FieldValue::OrSetTombstone(tag)` shape and dedupe
/// against it at fold time. The bead spec calls remove out of scope
/// for v1.
#[derive(Debug, Clone)]
pub struct OrSetAlgebra {
    field: FieldName,
}

impl OrSetAlgebra {
    pub fn new(field: FieldName) -> Self {
        Self { field }
    }

    fn extract_text(&self, v: &FieldValue) -> Result<String> {
        match v {
            FieldValue::String(s) => Ok(s.clone()),
            FieldValue::OptString(Some(s)) => Ok(s.clone()),
            other => Err(anyhow!(
                "OrSetAlgebra({:?}): expected String or OptString(Some), got {:?}",
                self.field,
                other
            )),
        }
    }
}

impl FieldAlgebra for OrSetAlgebra {
    fn field_name(&self) -> FieldName {
        self.field.clone()
    }

    fn fold(&self, obs: &[&Observation]) -> Result<FieldValue> {
        // Sort by (source, source_event_id) for deterministic output —
        // a JSON array's element order is observable by callers, so we
        // need a stable order or `reorder_invariance` (ADR-0010 §9)
        // would fail at the JSON-equality level even if the SET of
        // members is the same.
        let mut entries: Vec<serde_json::Value> = Vec::with_capacity(obs.len());
        for o in obs {
            let text = self.extract_text(&o.value)?;
            entries.push(json!({
                "source": o.source.as_str(),
                "source_event_id": o.source_event_id,
                "value": text,
                "observed_at": o.observed_at.to_rfc3339(),
            }));
        }
        entries.sort_by(|a, b| {
            let key_a = (
                a["source"].as_str().unwrap_or(""),
                a["source_event_id"].as_str().unwrap_or(""),
            );
            let key_b = (
                b["source"].as_str().unwrap_or(""),
                b["source_event_id"].as_str().unwrap_or(""),
            );
            key_a.cmp(&key_b)
        });
        Ok(FieldValue::Json(serde_json::Value::Array(entries)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{Source, WorkRef};
    use chrono::Utc;

    fn comment(text: &str, source: &str, evt: &str) -> Observation {
        Observation {
            work_item: WorkRef {
                repo: "rosary".to_string(),
                scope: String::new(),
                bead_id: "rosary-test".to_string(),
            },
            source: Source::new(source),
            source_event_id: evt.to_string(),
            field: FieldName::Comment,
            value: FieldValue::String(text.to_string()),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: format!("{source}-{evt}-{text}"),
        }
    }

    /// ADR-0010 invariant 7: or_set_unique_tags. The same payload from
    /// two sources is two distinct entries — `(source, event_id)` is
    /// the identity, not the value text.
    #[test]
    fn or_set_unique_tags() {
        let alg = OrSetAlgebra::new(FieldName::Comment);
        // Same comment text, different sources → 2 entries.
        let a = comment("LGTM", "linear", "evt-1");
        let b = comment("LGTM", "github", "evt-2");
        let r = alg.fold(&[&a, &b]).unwrap();
        let arr = match r {
            FieldValue::Json(serde_json::Value::Array(a)) => a,
            _ => panic!("expected JSON array"),
        };
        assert_eq!(arr.len(), 2, "same text, different sources → 2 entries");
        let sources: Vec<&str> = arr.iter().filter_map(|e| e["source"].as_str()).collect();
        assert!(sources.contains(&"github"));
        assert!(sources.contains(&"linear"));
    }

    /// ADR-0010 invariant 6: or_set_add_remove_commute. Phase 1 is
    /// add-only (no remove API) so this test verifies the relaxation:
    /// two adds in any order produce the same set.
    #[test]
    fn or_set_add_order_invariance() {
        let alg = OrSetAlgebra::new(FieldName::Comment);
        let a = comment("first", "src", "evt-1");
        let b = comment("second", "src", "evt-2");
        let r1 = alg.fold(&[&a, &b]).unwrap();
        let r2 = alg.fold(&[&b, &a]).unwrap();
        assert_eq!(r1, r2, "add order must not affect output");
    }

    #[test]
    fn or_set_empty_is_empty_array() {
        let alg = OrSetAlgebra::new(FieldName::Comment);
        let r = alg.fold(&[]).unwrap();
        match r {
            FieldValue::Json(serde_json::Value::Array(a)) => assert!(a.is_empty()),
            _ => panic!("expected empty JSON array"),
        }
    }

    #[test]
    fn or_set_preserves_provenance() {
        let alg = OrSetAlgebra::new(FieldName::Comment);
        let a = comment("hello", "alice", "evt-1");
        let r = alg.fold(&[&a]).unwrap();
        let arr = match r {
            FieldValue::Json(serde_json::Value::Array(a)) => a,
            _ => panic!(),
        };
        assert_eq!(arr.len(), 1);
        assert_eq!(arr[0]["source"], "alice");
        assert_eq!(arr[0]["source_event_id"], "evt-1");
        assert_eq!(arr[0]["value"], "hello");
    }

    #[test]
    fn or_set_type_mismatch_errors() {
        let alg = OrSetAlgebra::new(FieldName::Comment);
        let bad = Observation {
            work_item: WorkRef {
                repo: "r".to_string(),
                scope: String::new(),
                bead_id: "b".to_string(),
            },
            source: Source::new("src"),
            source_event_id: "evt-bad".to_string(),
            field: FieldName::Comment,
            value: FieldValue::Int64(42),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: "x".to_string(),
        };
        let r = alg.fold(&[&bad]);
        assert!(r.is_err());
    }

    #[test]
    fn or_set_field_name() {
        assert_eq!(
            OrSetAlgebra::new(FieldName::Label).field_name(),
            FieldName::Label
        );
    }
}
