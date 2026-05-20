//! Priority work queue with triage scoring and exponential backoff.
//!
//! Modeled after driftlessaf's workqueue: priority ordering, NotBefore scheduling,
//! dedup by bead ID, and bounded retry with exponential backoff.

use std::cmp::Ordering;
use std::collections::{BinaryHeap, HashMap, HashSet};
use std::time::{Duration, Instant};

use crate::bead::Bead;

/// Base backoff period (doubles per retry).
const BACKOFF_BASE: Duration = Duration::from_secs(30);
/// Maximum backoff period.
const BACKOFF_MAX: Duration = Duration::from_secs(30 * 60);
/// Default max retries before deadletter.
const MAX_RETRIES: u32 = 5;

/// An entry in the priority queue.
#[derive(Debug, Clone)]
pub struct QueueEntry {
    pub bead_id: String,
    pub repo: String,
    pub score: f64,
    pub enqueued_at: Instant,
    pub retries: u32,
    pub generation: u64,
}

// BinaryHeap is a max-heap; higher score = dequeued first.
// Ties broken by earlier enqueue time (older first = fairness).
//
// PartialEq is consistent with Ord: two entries are equal iff cmp returns Equal.
// Deduplication is handled externally by `in_queue: HashSet<(repo, bead_id)>`,
// so the heap never contains two entries with the same (repo, bead_id) pair.
impl PartialEq for QueueEntry {
    fn eq(&self, other: &Self) -> bool {
        self.cmp(other) == Ordering::Equal
    }
}

impl Eq for QueueEntry {}

impl PartialOrd for QueueEntry {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for QueueEntry {
    fn cmp(&self, other: &Self) -> Ordering {
        self.score
            .partial_cmp(&other.score)
            .unwrap_or(Ordering::Equal)
            .then_with(|| other.enqueued_at.cmp(&self.enqueued_at))
    }
}

/// Backoff state for a bead that has been retried.
#[derive(Debug, Clone)]
pub struct BackoffState {
    pub retries: u32,
    pub next_eligible: Instant,
}

impl BackoffState {
    pub fn new(retries: u32, now: Instant) -> Self {
        let delay = std::cmp::min(
            BACKOFF_BASE.saturating_mul(1 << retries.min(10)),
            BACKOFF_MAX,
        );
        BackoffState {
            retries,
            next_eligible: now + delay,
        }
    }

    pub fn is_eligible(&self, now: Instant) -> bool {
        now >= self.next_eligible
    }

    pub fn exceeded_max(&self) -> bool {
        self.retries >= MAX_RETRIES
    }
}

/// Priority work queue with dedup and backoff tracking.
pub struct WorkQueue {
    heap: BinaryHeap<QueueEntry>,
    in_queue: HashSet<(String, String)>,
    backoff: HashMap<(String, String), BackoffState>,
    /// Minimum priority level for auto-dispatch. Beads with priority > min_priority
    /// are blocked (P0=highest, P3=lowest). Default 2 means P0, P1, P2 pass; P3 blocked.
    pub min_priority: u8,
}

impl WorkQueue {
    pub fn new() -> Self {
        WorkQueue {
            heap: BinaryHeap::new(),
            in_queue: HashSet::new(),
            backoff: HashMap::new(),
            min_priority: 2,
        }
    }

    /// Enqueue a bead if not already in the queue.
    /// Returns true if the bead was added.
    pub fn enqueue(&mut self, entry: QueueEntry) -> bool {
        let key = (entry.repo.clone(), entry.bead_id.clone());
        if self.in_queue.contains(&key) {
            return false;
        }
        self.in_queue.insert(key);
        self.heap.push(entry);
        true
    }

    /// Dequeue the highest-priority bead that is eligible (past backoff).
    /// Skips entries still in backoff, leaving them in the queue.
    pub fn dequeue(&mut self, now: Instant) -> Option<QueueEntry> {
        let mut deferred = Vec::new();

        let result = loop {
            match self.heap.pop() {
                None => break None,
                Some(entry) => {
                    let key = (entry.repo.clone(), entry.bead_id.clone());
                    if let Some(state) = self.backoff.get(&key)
                        && !state.is_eligible(now)
                    {
                        deferred.push(entry);
                        continue;
                    }
                    self.in_queue.remove(&key);
                    break Some(entry);
                }
            }
        };

        // Put deferred entries back
        for entry in deferred {
            self.heap.push(entry);
        }

        result
    }

    /// Record a backoff for a failed bead.
    pub fn record_backoff(&mut self, repo: &str, bead_id: &str, retries: u32, now: Instant) {
        self.backoff.insert(
            (repo.to_string(), bead_id.to_string()),
            BackoffState::new(retries, now),
        );
    }

    /// Check if a bead has exceeded max retries.
    pub fn is_deadlettered(&self, repo: &str, bead_id: &str) -> bool {
        self.backoff
            .get(&(repo.to_string(), bead_id.to_string()))
            .is_some_and(|s| s.exceeded_max())
    }

    /// Get retry count for a bead.
    pub fn retries(&self, repo: &str, bead_id: &str) -> u32 {
        self.backoff
            .get(&(repo.to_string(), bead_id.to_string()))
            .map_or(0, |s| s.retries)
    }

    /// Number of entries currently in the queue.
    #[allow(dead_code)]
    pub fn len(&self) -> usize {
        self.heap.len()
    }

    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.heap.is_empty()
    }

    /// Check if a bead is already enqueued.
    #[allow(dead_code)]
    pub fn contains(&self, repo: &str, bead_id: &str) -> bool {
        self.in_queue
            .contains(&(repo.to_string(), bead_id.to_string()))
    }

    /// Check if a bead has pending backoff (scheduled for retry).
    pub fn has_backoff(&self, repo: &str, bead_id: &str) -> bool {
        self.backoff
            .contains_key(&(repo.to_string(), bead_id.to_string()))
    }

    /// Clear backoff state for a bead (e.g., on successful completion).
    pub fn clear_backoff(&mut self, repo: &str, bead_id: &str) {
        self.backoff
            .remove(&(repo.to_string(), bead_id.to_string()));
    }
}

/// Compute a triage score for a bead. Higher = more urgent.
///
/// Composite scoring (all factors 0.0–1.0):
/// - priority_score (0.4): lower priority number = higher score
/// - dependency_score (0.3): 1.0 if no deps, 0.0 if blocked
/// - age_score (0.2): ages up over 1 week
/// - retry_penalty (0.1): diminishing returns on retries
pub fn triage_score(bead: &Bead, retries: u32, now: chrono::DateTime<chrono::Utc>) -> f64 {
    let priority_score = 1.0 - (bead.priority as f64 / 5.0);

    let dependency_score = if bead.dependency_count == 0 { 1.0 } else { 0.0 };

    let age_hours = (now - bead.created_at).num_hours().max(0) as f64;
    let age_score = (age_hours / 168.0).min(1.0); // 168h = 1 week

    let retry_penalty = 1.0 / (1.0 + retries as f64);

    0.4 * priority_score + 0.3 * dependency_score + 0.2 * age_score + 0.1 * retry_penalty
}

/// Overnight triage: prefer small/mechanical beads that agents can complete.
/// Boosts bugs and chores, penalizes features/epics/design.
pub fn triage_score_overnight(
    bead: &Bead,
    retries: u32,
    now: chrono::DateTime<chrono::Utc>,
) -> f64 {
    let base = triage_score(bead, retries, now);

    // Issue type modifier: agents handle bugs/chores well, struggle with design
    let type_modifier = match bead.issue_type.as_str() {
        "bug" => 0.3,
        "chore" => 0.25,
        "task" => 0.1,
        "feature" => -0.1,
        "epic" => -0.5, // should be filtered anyway, but safety
        "design" | "research" | "review" => -0.3,
        _ => 0.0,
    };

    (base + type_modifier).clamp(0.0, 1.0)
}

/// Check whether a bead's priority passes the severity floor.
///
/// Priority numbering: 0 = P0 (highest urgency), 3 = P3 (lowest).
/// A floor of 2 means P0, P1, P2 pass (priority <= floor); P3 is blocked.
pub fn passes_severity_floor(bead: &Bead, floor: u8) -> bool {
    bead.priority <= floor
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn make_bead(id: &str, priority: u8, deps: u32, created_at: &str) -> Bead {
        Bead {
            id: id.to_string(),
            title: format!("bead {id}"),
            description: String::new(),
            status: "open".to_string(),
            priority,
            issue_type: "task".to_string(),
            owner: None,
            repo: "test".to_string(),
            created_at: created_at.parse().unwrap(),
            updated_at: created_at.parse().unwrap(),
            dependency_count: deps,
            dependent_count: 0,
            comment_count: 0,
            branch: None,
            pr_url: None,
            jj_change_id: None,
            external_ref: None,
            files: Vec::new(),
            test_files: Vec::new(),
            created_by: None,
            scope: String::new(),
            derived_from: vec![],
        }
    }

    #[test]
    fn higher_priority_dequeued_first() {
        let mut q = WorkQueue::new();
        let now = Instant::now();

        q.enqueue(QueueEntry {
            bead_id: "low".into(),
            repo: "r".into(),
            score: 0.3,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        });
        q.enqueue(QueueEntry {
            bead_id: "high".into(),
            repo: "r".into(),
            score: 0.9,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        });

        let first = q.dequeue(now).unwrap();
        assert_eq!(first.bead_id, "high");
        let second = q.dequeue(now).unwrap();
        assert_eq!(second.bead_id, "low");
    }

    #[test]
    fn dedup_prevents_double_enqueue() {
        let mut q = WorkQueue::new();
        let now = Instant::now();

        let entry = QueueEntry {
            bead_id: "x".into(),
            repo: "r".into(),
            score: 0.5,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        };

        assert!(q.enqueue(entry.clone()));
        assert!(!q.enqueue(entry));
        assert_eq!(q.len(), 1);
    }

    #[test]
    fn backoff_skips_ineligible() {
        let mut q = WorkQueue::new();
        let now = Instant::now();

        q.enqueue(QueueEntry {
            bead_id: "backed-off".into(),
            repo: "r".into(),
            score: 0.9,
            enqueued_at: now,
            retries: 1,
            generation: 0,
        });
        // Record backoff with future eligibility
        q.record_backoff("r", "backed-off", 1, now);

        // Should skip — still in backoff
        assert!(q.dequeue(now).is_none());
        // Entry should still be in queue
        assert_eq!(q.len(), 1);

        // After backoff expires, should dequeue
        let future = now + Duration::from_secs(120);
        let entry = q.dequeue(future).unwrap();
        assert_eq!(entry.bead_id, "backed-off");
    }

    #[test]
    fn backoff_exponential_growth() {
        let now = Instant::now();

        let b0 = BackoffState::new(0, now);
        let b1 = BackoffState::new(1, now);
        let b2 = BackoffState::new(2, now);
        let b5 = BackoffState::new(5, now);

        // Each retry doubles the delay
        let d0 = b0.next_eligible.duration_since(now);
        let d1 = b1.next_eligible.duration_since(now);
        let d2 = b2.next_eligible.duration_since(now);

        assert_eq!(d0, Duration::from_secs(30));
        assert_eq!(d1, Duration::from_secs(60));
        assert_eq!(d2, Duration::from_secs(120));

        // Should cap at BACKOFF_MAX
        let d5 = b5.next_eligible.duration_since(now);
        assert!(d5 <= BACKOFF_MAX);
    }

    #[test]
    fn max_retries_deadletters() {
        let mut q = WorkQueue::new();
        let now = Instant::now();

        q.record_backoff("r", "doomed", MAX_RETRIES, now);
        assert!(q.is_deadlettered("r", "doomed"));

        q.record_backoff("r", "ok", 1, now);
        assert!(!q.is_deadlettered("r", "ok"));
    }

    #[test]
    fn clear_backoff_resets() {
        let mut q = WorkQueue::new();
        let now = Instant::now();

        q.record_backoff("r", "x", 3, now);
        assert_eq!(q.retries("r", "x"), 3);

        q.clear_backoff("r", "x");
        assert_eq!(q.retries("r", "x"), 0);
        assert!(!q.is_deadlettered("r", "x"));
    }

    #[test]
    fn triage_score_priority_dominates() {
        let now = chrono::Utc::now();
        let high = make_bead("h", 0, 0, "2026-03-12T00:00:00Z");
        let low = make_bead("l", 4, 0, "2026-03-12T00:00:00Z");

        let sh = triage_score(&high, 0, now);
        let sl = triage_score(&low, 0, now);
        assert!(sh > sl, "P0 should score higher than P4: {sh} vs {sl}");
    }

    #[test]
    fn triage_score_blocked_beads_low() {
        let now = chrono::Utc::now();
        let ready = make_bead("r", 2, 0, "2026-03-12T00:00:00Z");
        let blocked = make_bead("b", 2, 3, "2026-03-12T00:00:00Z");

        let sr = triage_score(&ready, 0, now);
        let sb = triage_score(&blocked, 0, now);
        assert!(
            sr > sb,
            "ready should score higher than blocked: {sr} vs {sb}"
        );
    }

    #[test]
    fn triage_score_age_increases() {
        let now = chrono::Utc.with_ymd_and_hms(2026, 3, 19, 0, 0, 0).unwrap();
        let old = make_bead("old", 2, 0, "2026-03-05T00:00:00Z"); // 14 days ago
        let new = make_bead("new", 2, 0, "2026-03-18T00:00:00Z"); // 1 day ago

        let so = triage_score(&old, 0, now);
        let sn = triage_score(&new, 0, now);
        assert!(so > sn, "older bead should score higher: {so} vs {sn}");
    }

    #[test]
    fn triage_score_retries_penalize() {
        let now = chrono::Utc::now();
        let bead = make_bead("x", 2, 0, "2026-03-12T00:00:00Z");

        let s0 = triage_score(&bead, 0, now);
        let s3 = triage_score(&bead, 3, now);
        assert!(
            s0 > s3,
            "0 retries should score higher than 3: {s0} vs {s3}"
        );
    }

    #[test]
    fn older_entry_wins_tiebreak() {
        let mut q = WorkQueue::new();
        let now = Instant::now();
        let later = now + Duration::from_secs(1);

        q.enqueue(QueueEntry {
            bead_id: "newer".into(),
            repo: "r".into(),
            score: 0.5,
            enqueued_at: later,
            retries: 0,
            generation: 0,
        });
        q.enqueue(QueueEntry {
            bead_id: "older".into(),
            repo: "r".into(),
            score: 0.5,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        });

        let first = q.dequeue(now).unwrap();
        assert_eq!(first.bead_id, "older");
    }

    #[test]
    fn severity_floor_blocks_low_priority() {
        let bead = make_bead("p3", 3, 0, "2026-03-12T00:00:00Z");
        assert!(
            !passes_severity_floor(&bead, 2),
            "P3 bead should be blocked by floor=2"
        );
    }

    #[test]
    fn severity_floor_passes_high_priority() {
        let bead = make_bead("p1", 1, 0, "2026-03-12T00:00:00Z");
        assert!(
            passes_severity_floor(&bead, 2),
            "P1 bead should pass floor=2"
        );
    }

    #[test]
    fn severity_floor_edge_at_boundary() {
        let bead = make_bead("p2", 2, 0, "2026-03-12T00:00:00Z");
        assert!(
            passes_severity_floor(&bead, 2),
            "P2 bead should pass floor=2 (boundary is inclusive)"
        );
    }

    #[test]
    fn age_does_not_override_severity_floor() {
        // An old P3 bead should still be blocked by the severity floor,
        // regardless of how high its age-based triage score would be.
        let bead = make_bead("old-p3", 3, 0, "2025-01-01T00:00:00Z");
        assert!(
            !passes_severity_floor(&bead, 2),
            "old P3 bead should still be blocked by floor=2"
        );
        // Also verify it would have a nonzero triage score (age contributes),
        // confirming that age alone cannot bypass the floor.
        let now = chrono::Utc::now();
        let score = triage_score(&bead, 0, now);
        assert!(
            score > 0.0,
            "old bead has positive triage score ({score}) but floor still blocks it"
        );
    }

    #[test]
    fn work_queue_default_min_priority() {
        let q = WorkQueue::new();
        assert_eq!(q.min_priority, 2, "default min_priority should be 2");
    }

    #[test]
    fn overnight_score_prefers_bugs_over_features() {
        let now = chrono::Utc::now();
        let bug = Bead {
            issue_type: "bug".to_string(),
            ..make_bead("bug-1", 1, 0, "2026-03-13T00:00:00Z")
        };
        let feature = Bead {
            issue_type: "feature".to_string(),
            ..make_bead("feat-1", 1, 0, "2026-03-13T00:00:00Z")
        };

        let bug_score = triage_score_overnight(&bug, 0, now);
        let feat_score = triage_score_overnight(&feature, 0, now);
        assert!(
            bug_score > feat_score,
            "overnight should prefer bug ({bug_score}) over feature ({feat_score})"
        );
    }

    #[test]
    fn overnight_score_penalizes_epics() {
        let now = chrono::Utc::now();
        let task = Bead {
            issue_type: "task".to_string(),
            ..make_bead("task-1", 2, 0, "2026-03-13T00:00:00Z")
        };
        let epic = Bead {
            issue_type: "epic".to_string(),
            ..make_bead("epic-1", 2, 0, "2026-03-13T00:00:00Z")
        };

        let task_score = triage_score_overnight(&task, 0, now);
        let epic_score = triage_score_overnight(&epic, 0, now);
        assert!(
            task_score > epic_score,
            "overnight should prefer task ({task_score}) over epic ({epic_score})"
        );
    }

    #[test]
    fn same_bead_id_different_repos_both_enqueued() {
        // Regression: prior to composite-key fix, the second enqueue would be
        // rejected as a duplicate because in_queue was keyed on bead_id alone.
        let mut q = WorkQueue::new();
        let now = Instant::now();

        let added1 = q.enqueue(QueueEntry {
            bead_id: "shared-id".into(),
            repo: "repo-a".into(),
            score: 0.5,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        });
        let added2 = q.enqueue(QueueEntry {
            bead_id: "shared-id".into(),
            repo: "repo-b".into(),
            score: 0.5,
            enqueued_at: now,
            retries: 0,
            generation: 0,
        });

        assert!(added1, "first enqueue should succeed");
        assert!(
            added2,
            "second enqueue with same bead_id but different repo should succeed"
        );
        assert_eq!(q.len(), 2, "both entries should be in the queue");
    }
}
