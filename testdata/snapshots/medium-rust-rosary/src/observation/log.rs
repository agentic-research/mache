//! In-memory observation log + content-hash dedup before fold.
//!
//! Bead `obs-storage-and-quarantine` (rosary-97e386). ADR-0010
//! invariant 8 (`dedup_before_fold`).
//!
//! Phase 1 is in-memory only — Dolt schema for the persistent log
//! comes in Phase 2 once the algebra surface is settled. The log is a
//! G-set keyed by `DedupKey = (Source, source_event_id, payload_hash)`:
//! inserts of the same key are no-ops, so a webhook replay or a
//! double-emitted poll counts once regardless of how many times it
//! arrives.

use std::collections::BTreeMap;

use super::{DedupKey, FieldName, Observation, WorkRef};

/// In-memory observation log.
///
/// `BTreeMap` keyed on `DedupKey` so iteration is deterministic
/// (lexicographic by source, then event_id, then payload_hash) — that
/// determinism is what makes the fold reorder-invariant in practice
/// even though algebras must satisfy reorder invariance independently.
#[derive(Debug, Default, Clone)]
pub struct ObservationLog {
    by_key: BTreeMap<DedupKey, Observation>,
}

impl ObservationLog {
    pub fn new() -> Self {
        Self::default()
    }

    /// Insert an observation. Idempotent on `DedupKey` collision —
    /// returns `false` if the key already existed (insert was a no-op),
    /// `true` if this is a fresh entry. The first-write-wins on the
    /// stored payload (we don't update the existing row); both
    /// observations are byte-identical by definition (same payload_hash)
    /// so this is observationally a no-op.
    pub fn insert(&mut self, obs: Observation) -> bool {
        let key = obs.dedup_key();
        if self.by_key.contains_key(&key) {
            return false;
        }
        self.by_key.insert(key, obs);
        true
    }

    /// Total observation count (post-dedup).
    pub fn len(&self) -> usize {
        self.by_key.len()
    }

    pub fn is_empty(&self) -> bool {
        self.by_key.is_empty()
    }

    /// Iterate observations in dedup-key order. Stable across calls.
    pub fn iter(&self) -> impl Iterator<Item = &Observation> {
        self.by_key.values()
    }

    /// All observations on a single (work_item, field) pair, in
    /// dedup-key order. The fold uses this to feed an algebra a slice
    /// of just-this-field-on-this-bead observations.
    pub fn for_field<'a>(
        &'a self,
        work_item: &'a WorkRef,
        field: &'a FieldName,
    ) -> Vec<&'a Observation> {
        self.by_key
            .values()
            .filter(|o| &o.work_item == work_item && &o.field == field)
            .collect()
    }

    /// All observations on a single work_item across fields, grouped
    /// by `(field, source)` — useful for downstream cross-source
    /// derivation (which feeds the flat-lattice ⊤ check).
    pub fn for_work_item<'a>(&'a self, work_item: &'a WorkRef) -> Vec<&'a Observation> {
        self.by_key
            .values()
            .filter(|o| &o.work_item == work_item)
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{FieldValue, Source};
    use chrono::Utc;

    fn obs(source: &str, evt: &str, payload_hash: &str) -> Observation {
        Observation {
            work_item: WorkRef {
                repo: "r".to_string(),
                scope: String::new(),
                bead_id: "b".to_string(),
            },
            source: Source::new(source),
            source_event_id: evt.to_string(),
            field: FieldName::Assignee,
            value: FieldValue::OptString(Some("alice".to_string())),
            observed_at: Utc::now(),
            cert: None,
            payload_hash: payload_hash.to_string(),
        }
    }

    /// ADR-0010 invariant 8: dedup_before_fold. Replaying the same
    /// `(source, source_event_id, payload_hash)` is a no-op.
    #[test]
    fn dedup_before_fold() {
        let mut log = ObservationLog::new();
        let first = log.insert(obs("github", "evt-1", "hash-a"));
        let second = log.insert(obs("github", "evt-1", "hash-a"));
        assert!(first, "first insert should report fresh");
        assert!(!second, "duplicate insert should be a no-op");
        assert_eq!(log.len(), 1);
    }

    #[test]
    fn dedup_distinguishes_payload_hash() {
        // Same (source, event_id) but different payload_hash → 2 entries.
        // This case shouldn't happen in practice (event_id is supposed
        // to be unique per-source) but the dedup key is the full triple
        // so both rows survive — surfacing source-bug as observable
        // history rather than silent data loss.
        let mut log = ObservationLog::new();
        log.insert(obs("github", "evt-1", "hash-a"));
        log.insert(obs("github", "evt-1", "hash-b"));
        assert_eq!(log.len(), 2);
    }

    #[test]
    fn iter_order_is_deterministic() {
        let mut log1 = ObservationLog::new();
        log1.insert(obs("github", "z", "h1"));
        log1.insert(obs("linear", "a", "h2"));
        log1.insert(obs("bead", "m", "h3"));

        let mut log2 = ObservationLog::new();
        log2.insert(obs("bead", "m", "h3"));
        log2.insert(obs("linear", "a", "h2"));
        log2.insert(obs("github", "z", "h1"));

        let order1: Vec<&str> = log1.iter().map(|o| o.source.as_str()).collect();
        let order2: Vec<&str> = log2.iter().map(|o| o.source.as_str()).collect();
        assert_eq!(
            order1, order2,
            "iter order must be insertion-order-independent"
        );
    }

    #[test]
    fn for_field_filters_correctly() {
        let mut log = ObservationLog::new();
        let o1 = obs("github", "evt-1", "h1");
        let mut o2 = obs("linear", "evt-2", "h2");
        o2.field = FieldName::PrUrl;
        log.insert(o1);
        log.insert(o2);

        let work = WorkRef {
            repo: "r".to_string(),
            scope: String::new(),
            bead_id: "b".to_string(),
        };
        let assignees = log.for_field(&work, &FieldName::Assignee);
        assert_eq!(assignees.len(), 1);
        let pr_urls = log.for_field(&work, &FieldName::PrUrl);
        assert_eq!(pr_urls.len(), 1);
    }

    #[test]
    fn empty_log_is_is_empty() {
        let log = ObservationLog::new();
        assert!(log.is_empty());
        assert_eq!(log.len(), 0);
    }
}
