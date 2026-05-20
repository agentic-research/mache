//! SQLite-backed observation log + quarantine.
//!
//! Phase 2 of ADR-0010 (rosary-7023a9 substrate). The Phase 1
//! [`super::log::ObservationLog`] is in-memory only; this module
//! persists the same G-set + quarantine to the orchestrator's
//! `~/.rsry/backend.db` so observations survive restarts.
//!
//! Schema lives in `src/store_sqlite.rs::SCHEMA` (the orchestrator
//! schema, not per-repo bead state). Two tables: `observations`
//! (the G-set) and `observation_quarantine`. Dedup is the unique
//! index `(source, source_event_id, payload_hash)` — replayed
//! webhooks hit `INSERT OR IGNORE` and are a structural no-op.
//!
//! API mirrors [`super::log::ObservationLog`] so callers can swap
//! the backing store without code change. The substrate's algebras
//! and fold operate on slices of [`super::Observation`] — they
//! don't care which backend produced the slice.

use anyhow::{Context, Result};
use rusqlite::{Connection, params};
use std::path::Path;
use std::sync::Mutex;

use super::quarantine::{QuarantineEntry, QuarantineReason};
use super::{FieldName, Observation, Source};
use crate::store::WorkRef;

/// SQLite-backed observation log.
pub struct SqliteObservationLog {
    conn: Mutex<Connection>,
}

impl SqliteObservationLog {
    /// Open an observation log at the given path. Schema is applied
    /// idempotently on first open.
    pub fn open(path: &Path) -> Result<Self> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).with_context(|| {
                format!(
                    "creating parent dir for observation log at {}",
                    path.display()
                )
            })?;
        }
        let conn = Connection::open(path)
            .with_context(|| format!("opening observation log: {}", path.display()))?;
        conn.execute_batch("PRAGMA journal_mode=WAL;")
            .context("enabling WAL on observation log")?;
        // Run the orchestrator schema — picks up the `observations` +
        // `observation_quarantine` tables defined alongside everything
        // else (decades, threads, etc).
        conn.execute_batch(crate::store_sqlite::SCHEMA)
            .context("applying observation log schema")?;
        Ok(SqliteObservationLog {
            conn: Mutex::new(conn),
        })
    }

    /// Insert an observation. Returns `Ok(true)` if newly inserted,
    /// `Ok(false)` if a row with the same dedup key already existed
    /// (no-op, ADR-0010 invariant 8).
    pub fn insert(&self, obs: &Observation) -> Result<bool> {
        let value_json = serde_json::to_string(&obs.value).context("serialize FieldValue")?;
        let cert_json = match &obs.cert {
            Some(c) => Some(serde_json::to_string(c).context("serialize SignetCert")?),
            None => None,
        };
        let field_str = serde_json::to_string(&obs.field).context("serialize FieldName")?;
        // Strip the JSON quoting on simple variants — `"pipeline_verdict"`
        // becomes `pipeline_verdict`. For `Other("x")` it stays as
        // `{"other":"x"}`. Either form roundtrips cleanly via
        // serde_json::from_str on read.

        let conn = self.conn.lock().unwrap();
        let n = conn
            .execute(
                "INSERT OR IGNORE INTO observations
                 (repo, scope, bead_id, source, source_event_id,
                  field, value_json, observed_at, cert_json, payload_hash)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
                params![
                    obs.work_item.repo,
                    obs.work_item.scope,
                    obs.work_item.bead_id,
                    obs.source.as_str(),
                    obs.source_event_id,
                    field_str,
                    value_json,
                    obs.observed_at.to_rfc3339(),
                    cert_json,
                    obs.payload_hash,
                ],
            )
            .context("insert observation")?;
        Ok(n > 0)
    }

    /// Insert a quarantined observation. Always succeeds; quarantine
    /// is the safety valve, never the bottleneck.
    pub fn quarantine(&self, obs: &Observation, reason: &QuarantineReason) -> Result<()> {
        let observation_json = serde_json::to_string(obs).context("serialize observation")?;
        let reason_json = serde_json::to_string(reason).context("serialize reason")?;
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO observation_quarantine (observation_json, reason_json)
             VALUES (?1, ?2)",
            params![observation_json, reason_json],
        )
        .context("insert quarantine entry")?;
        Ok(())
    }

    /// Total observation count.
    pub fn len(&self) -> Result<usize> {
        let conn = self.conn.lock().unwrap();
        let n: i64 = conn
            .query_row("SELECT COUNT(*) FROM observations", [], |row| row.get(0))
            .context("count observations")?;
        Ok(n as usize)
    }

    /// All observations on a single (work_item, field) pair, ordered
    /// by `observed_at, source, source_event_id` for determinism.
    pub fn for_field(&self, work_item: &WorkRef, field: &FieldName) -> Result<Vec<Observation>> {
        let conn = self.conn.lock().unwrap();
        let field_str = serde_json::to_string(field).context("serialize FieldName")?;
        let mut stmt = conn
            .prepare(
                "SELECT repo, scope, bead_id, source, source_event_id,
                        field, value_json, observed_at, cert_json, payload_hash
                 FROM observations
                 WHERE repo = ?1 AND scope = ?2 AND bead_id = ?3 AND field = ?4
                 ORDER BY observed_at, source, source_event_id",
            )
            .context("prepare for_field query")?;
        let rows = stmt
            .query_map(
                params![
                    work_item.repo,
                    work_item.scope,
                    work_item.bead_id,
                    field_str,
                ],
                row_to_observation,
            )
            .context("execute for_field query")?
            .collect::<rusqlite::Result<Vec<Observation>>>()
            .context("read for_field results")?;
        Ok(rows)
    }

    /// All observations on a single work_item across fields.
    pub fn for_work_item(&self, work_item: &WorkRef) -> Result<Vec<Observation>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn
            .prepare(
                "SELECT repo, scope, bead_id, source, source_event_id,
                        field, value_json, observed_at, cert_json, payload_hash
                 FROM observations
                 WHERE repo = ?1 AND scope = ?2 AND bead_id = ?3
                 ORDER BY observed_at, source, source_event_id",
            )
            .context("prepare for_work_item query")?;
        let rows = stmt
            .query_map(
                params![work_item.repo, work_item.scope, work_item.bead_id],
                row_to_observation,
            )
            .context("execute for_work_item query")?
            .collect::<rusqlite::Result<Vec<Observation>>>()
            .context("read for_work_item results")?;
        Ok(rows)
    }

    /// Quarantined entries — surfaced via `rsry status --quarantine`
    /// (Phase 2 user-facing). Always returned: quarantine is queryable,
    /// never silently dropped (ADR-0010 invariant 12).
    pub fn iter_quarantined(&self) -> Result<Vec<QuarantineEntry>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn
            .prepare(
                "SELECT observation_json, reason_json, quarantined_at
                 FROM observation_quarantine ORDER BY quarantined_at, id",
            )
            .context("prepare quarantine query")?;
        let rows = stmt
            .query_map([], |row| {
                let obs_json: String = row.get(0)?;
                let reason_json: String = row.get(1)?;
                let quarantined_at: String = row.get(2)?;
                let observation: Observation = serde_json::from_str(&obs_json).map_err(|e| {
                    rusqlite::Error::FromSqlConversionFailure(
                        0,
                        rusqlite::types::Type::Text,
                        Box::new(e),
                    )
                })?;
                let reason: QuarantineReason = serde_json::from_str(&reason_json).map_err(|e| {
                    rusqlite::Error::FromSqlConversionFailure(
                        0,
                        rusqlite::types::Type::Text,
                        Box::new(e),
                    )
                })?;
                let parsed_at = chrono::DateTime::parse_from_rfc3339(&quarantined_at)
                    .map(|d| d.with_timezone(&chrono::Utc))
                    .unwrap_or_else(|_| {
                        chrono::NaiveDateTime::parse_from_str(&quarantined_at, "%Y-%m-%d %H:%M:%S")
                            .map(|n| chrono::TimeZone::from_utc_datetime(&chrono::Utc, &n))
                            .unwrap_or_else(|_| chrono::Utc::now())
                    });
                Ok(QuarantineEntry {
                    observation,
                    reason,
                    quarantined_at: parsed_at,
                })
            })
            .context("execute quarantine query")?
            .collect::<rusqlite::Result<Vec<QuarantineEntry>>>()
            .context("read quarantine results")?;
        Ok(rows)
    }
}

/// Translate a row in the `observations` table back to an in-memory
/// `Observation`. Used by every read path; keeping it as a free
/// function avoids duplicating the column ordering across queries.
fn row_to_observation(row: &rusqlite::Row<'_>) -> rusqlite::Result<Observation> {
    let value_json: String = row.get("value_json")?;
    let value = serde_json::from_str(&value_json).map_err(|e| {
        rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(e))
    })?;
    let field_str: String = row.get("field")?;
    let field = serde_json::from_str(&field_str).map_err(|e| {
        rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(e))
    })?;
    let cert_json: Option<String> = row.get("cert_json")?;
    let cert = match cert_json {
        Some(s) => Some(serde_json::from_str(&s).map_err(|e| {
            rusqlite::Error::FromSqlConversionFailure(0, rusqlite::types::Type::Text, Box::new(e))
        })?),
        None => None,
    };
    let observed_at_str: String = row.get("observed_at")?;
    let observed_at = chrono::DateTime::parse_from_rfc3339(&observed_at_str)
        .map(|d| d.with_timezone(&chrono::Utc))
        .unwrap_or_else(|_| chrono::Utc::now());

    Ok(Observation {
        work_item: WorkRef {
            repo: row.get("repo")?,
            scope: row.get("scope")?,
            bead_id: row.get("bead_id")?,
        },
        source: Source::new(row.get::<_, String>("source")?),
        source_event_id: row.get("source_event_id")?,
        field,
        value,
        observed_at,
        cert,
        payload_hash: row.get("payload_hash")?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::observation::{FieldName, FieldValue, PipelineVerdictValue, SignetCert};
    use chrono::{DateTime, Utc};
    use tempfile::TempDir;

    fn at(secs: i64) -> DateTime<Utc> {
        DateTime::from_timestamp(secs, 0).unwrap()
    }

    fn workref(id: &str) -> WorkRef {
        WorkRef {
            repo: "rosary".to_string(),
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

    fn open_log() -> (TempDir, SqliteObservationLog) {
        let tmp = TempDir::new().unwrap();
        let log = SqliteObservationLog::open(&tmp.path().join("obs.db")).unwrap();
        (tmp, log)
    }

    #[test]
    fn insert_and_count() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let o = obs(
            &w,
            "github",
            "evt-1",
            FieldName::Assignee,
            FieldValue::OptString(Some("alice".to_string())),
            at(1000),
        );
        assert_eq!(log.len().unwrap(), 0);
        assert!(log.insert(&o).unwrap());
        assert_eq!(log.len().unwrap(), 1);
    }

    /// ADR-0010 invariant 8: dedup_before_fold (now persistent).
    #[test]
    fn dedup_via_unique_index() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let o = obs(
            &w,
            "github",
            "evt-replay",
            FieldName::Assignee,
            FieldValue::OptString(Some("alice".to_string())),
            at(1000),
        );
        assert!(log.insert(&o).unwrap(), "first insert is fresh");
        assert!(!log.insert(&o).unwrap(), "duplicate is no-op");
        assert!(!log.insert(&o).unwrap(), "n-th replay is also no-op");
        assert_eq!(log.len().unwrap(), 1);
    }

    #[test]
    fn round_trip_all_field_value_variants() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let cases: Vec<(FieldName, FieldValue)> = vec![
            (
                FieldName::Assignee,
                FieldValue::OptString(Some("alice".to_string())),
            ),
            (FieldName::PrUrl, FieldValue::OptString(None)),
            (FieldName::Ahead, FieldValue::Int64(7)),
            (FieldName::Deadline, FieldValue::Timestamp(at(99999))),
            (
                FieldName::PipelineVerdict,
                FieldValue::PipelineVerdict(PipelineVerdictValue::Pass),
            ),
            (FieldName::Comment, FieldValue::String("hello".to_string())),
            (FieldName::Status, FieldValue::String("Done".to_string())),
        ];
        for (i, (f, v)) in cases.iter().enumerate() {
            let mut o = obs(
                &w,
                "src",
                &format!("evt-{i}"),
                f.clone(),
                v.clone(),
                at(1000 + i as i64),
            );
            o.payload_hash = format!("ph-{i}");
            log.insert(&o).unwrap();
        }
        let read_back = log.for_work_item(&w).unwrap();
        assert_eq!(read_back.len(), cases.len());

        // Field-value typed values must round-trip exactly — that's
        // the whole point of having the typed sum.
        for o in &read_back {
            let original = cases.iter().find(|(f, _)| f == &o.field);
            let original = original.expect("field must be in cases");
            assert_eq!(o.value, original.1, "value round-trip for {:?}", o.field);
        }
    }

    #[test]
    fn round_trip_with_cert() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let mut o = obs(
            &w,
            "user",
            "evt-cert",
            FieldName::Assignee,
            FieldValue::OptString(Some("alice".to_string())),
            at(1000),
        );
        o.cert = Some(SignetCert {
            key_id: "abc123".to_string(),
            signature: "sig-base64".to_string(),
        });
        log.insert(&o).unwrap();

        let read_back = log.for_work_item(&w).unwrap();
        assert_eq!(read_back.len(), 1);
        assert_eq!(read_back[0].cert, o.cert);
    }

    #[test]
    fn for_field_filters_correctly() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        log.insert(&obs(
            &w,
            "src",
            "e1",
            FieldName::Assignee,
            FieldValue::OptString(Some("alice".to_string())),
            at(1000),
        ))
        .unwrap();
        log.insert(&obs(
            &w,
            "src",
            "e2",
            FieldName::PrUrl,
            FieldValue::OptString(Some("https://example/pr/1".to_string())),
            at(2000),
        ))
        .unwrap();

        let assignees = log.for_field(&w, &FieldName::Assignee).unwrap();
        assert_eq!(assignees.len(), 1);
        assert_eq!(assignees[0].field, FieldName::Assignee);

        let pr_urls = log.for_field(&w, &FieldName::PrUrl).unwrap();
        assert_eq!(pr_urls.len(), 1);
        assert_eq!(pr_urls[0].field, FieldName::PrUrl);
    }

    #[test]
    fn quarantine_round_trip() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let mut bad = obs(
            &w,
            "user",
            "evt-bad",
            FieldName::Assignee,
            FieldValue::OptString(Some("evil".to_string())),
            at(1000),
        );
        bad.cert = Some(SignetCert {
            key_id: "bogus".to_string(),
            signature: "deadbeef".to_string(),
        });
        let reason = QuarantineReason::InvalidCert {
            detail: "signature mismatch".to_string(),
        };
        log.quarantine(&bad, &reason).unwrap();

        let entries = log.iter_quarantined().unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].observation.source.as_str(), "user");
        assert_eq!(entries[0].reason, reason);
        assert!(matches!(
            entries[0].reason,
            QuarantineReason::InvalidCert { .. }
        ));
    }

    /// Quarantined obs MUST NOT show up in regular for_work_item /
    /// for_field reads — those queries hit the `observations` table,
    /// not `observation_quarantine` (ADR-0010 invariant 11).
    #[test]
    fn quarantine_does_not_leak_into_observations_reads() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        let bad = obs(
            &w,
            "user",
            "evt-bad",
            FieldName::Assignee,
            FieldValue::OptString(Some("evil".to_string())),
            at(1000),
        );
        log.quarantine(
            &bad,
            &QuarantineReason::InvalidCert {
                detail: "test".to_string(),
            },
        )
        .unwrap();

        // No insertion to `observations` happened — for_work_item
        // returns empty.
        let live = log.for_work_item(&w).unwrap();
        assert!(live.is_empty());
        assert_eq!(log.len().unwrap(), 0);
        // But the quarantine surface still shows it.
        assert_eq!(log.iter_quarantined().unwrap().len(), 1);
    }

    #[test]
    fn ordering_is_observed_at_then_source() {
        let (_tmp, log) = open_log();
        let w = workref("b1");
        // Insert in a deliberately scrambled order.
        log.insert(&obs(
            &w,
            "src-c",
            "e1",
            FieldName::Comment,
            FieldValue::String("c1".to_string()),
            at(2000),
        ))
        .unwrap();
        log.insert(&obs(
            &w,
            "src-a",
            "e2",
            FieldName::Comment,
            FieldValue::String("a1".to_string()),
            at(1000),
        ))
        .unwrap();
        log.insert(&obs(
            &w,
            "src-b",
            "e3",
            FieldName::Comment,
            FieldValue::String("b1".to_string()),
            at(1500),
        ))
        .unwrap();

        let read_back = log.for_field(&w, &FieldName::Comment).unwrap();
        let sources: Vec<&str> = read_back.iter().map(|o| o.source.as_str()).collect();
        // Ordered by observed_at — earliest first.
        assert_eq!(sources, vec!["src-a", "src-b", "src-c"]);
    }
}
