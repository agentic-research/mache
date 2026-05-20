//! Semantic grouping engine — clusters related beads into epics.
//!
//! Goes beyond Jaccard dedup: detects phases of the same work, sequential steps,
//! and overlapping scope. Uses multi-signal similarity (title tokens, description
//! overlap, shared scope prefixes, sequential numbering patterns) instead of
//! embeddings — no external service required.
//!
//! Actions: group into epic with ordering, merge near-duplicates preserving
//! context from both, suggest priority adjustments based on cluster.

use std::collections::{HashMap, HashSet};

use crate::bead::Bead;
use crate::scanner::jaccard_similarity;

/// A cluster of semantically related beads.
#[derive(Debug, Clone)]
pub struct BeadCluster {
    /// IDs of beads in this cluster, ordered by suggested execution sequence.
    pub bead_ids: Vec<String>,
    /// Why these beads were grouped.
    pub relationship: ClusterRelationship,
    /// Suggested action for the reconciler.
    pub action: ClusterAction,
    /// Combined similarity score (0.0–1.0) for the cluster.
    pub cohesion: f64,
}

/// How beads in a cluster relate to each other.
#[derive(Debug, Clone, PartialEq)]
pub enum ClusterRelationship {
    /// Near-duplicate titles/descriptions (Jaccard > 0.8).
    NearDuplicate,
    /// Sequential steps of the same work (step 1/2, phase A/B).
    Sequential,
    /// Shared scope prefix (e.g., "Auth: fix login", "Auth: fix signup").
    SharedScope,
    /// Overlapping content in title + description.
    Overlapping,
}

/// What the reconciler should do with a cluster.
#[derive(Debug, Clone, PartialEq)]
pub enum ClusterAction {
    /// Merge near-duplicates: keep the higher-priority one, append context from others.
    Merge {
        /// ID of the bead to keep.
        keep: String,
        /// IDs of beads to close as duplicates.
        close: Vec<String>,
    },
    /// Group as an ordered epic: dispatch in sequence, not in parallel.
    Sequence,
    /// Suppress: don't dispatch a bead if a cluster-mate is already active.
    Suppress,
}

// ---------------------------------------------------------------------------
// Stopword filtering for dedup
// ---------------------------------------------------------------------------

/// Common action verbs and filler words that appear in many bead titles but
/// carry no semantic signal for dedup purposes. Stripping these prevents
/// false-positive clustering of unrelated refactoring beads that happen to
/// share verbs like "fix", "add", "extract".
const TITLE_STOPWORDS: &[&str] = &[
    // action verbs
    "fix",
    "add",
    "replace",
    "extract",
    "implement",
    "remove",
    "update",
    "refactor",
    "consolidate",
    "move",
    "rename",
    "clean",
    "cleanup",
    "use",
    "make",
    "convert",
    "change",
    "improve",
    "handle",
    // articles / prepositions / conjunctions
    "the",
    "a",
    "an",
    "in",
    "to",
    "for",
    "of",
    "with",
    "from",
    "and",
    "on",
    "into",
    "by",
    "up",
];

/// Jaccard similarity with stopwords removed from both inputs.
/// This avoids inflated scores when beads share only common verbs.
fn jaccard_filtered(a: &str, b: &str) -> f64 {
    let stops: HashSet<&str> = TITLE_STOPWORDS.iter().copied().collect();
    let set_a: HashSet<String> = a
        .split_whitespace()
        .map(|w| w.to_lowercase())
        .filter(|w| !stops.contains(w.as_str()))
        .collect();
    let set_b: HashSet<String> = b
        .split_whitespace()
        .map(|w| w.to_lowercase())
        .filter(|w| !stops.contains(w.as_str()))
        .collect();

    if set_a.is_empty() && set_b.is_empty() {
        return 0.0; // all-stopword titles are not similar
    }

    let intersection = set_a.intersection(&set_b).count() as f64;
    let union = set_a.union(&set_b).count() as f64;

    if union == 0.0 {
        return 0.0;
    }

    intersection / union
}

// ---------------------------------------------------------------------------
// Multi-signal similarity
// ---------------------------------------------------------------------------

/// Compute a combined similarity score between two beads using multiple signals.
/// Returns a value between 0.0 and 1.0.
fn combined_similarity(a: &Bead, b: &Bead) -> f64 {
    // Signal 1: Title token overlap (Jaccard with stopwords stripped)
    let title_sim = jaccard_filtered(&a.title, &b.title);

    // Signal 2: Description token overlap
    let desc_sim = if a.description.is_empty() && b.description.is_empty() {
        0.0 // No signal, don't reward empty descriptions matching
    } else {
        jaccard_similarity(&a.description, &b.description)
    };

    // Signal 3: Shared scope prefix
    let scope_sim = scope_prefix_similarity(&a.title, &b.title);

    // Signal 4: Sequential pattern
    let seq_sim = sequential_similarity(&a.title, &b.title);

    // Weighted combination of all signals
    let weighted = 0.4 * title_sim + 0.2 * desc_sim + 0.2 * scope_sim + 0.2 * seq_sim;

    // Any strong individual signal should be sufficient — don't let absent
    // secondary signals dilute a clear match (e.g., high title overlap with
    // empty descriptions and no scope prefix).
    let best_signal = title_sim.max(scope_sim).max(seq_sim);

    weighted.max(best_signal * 0.85)
}

/// Detect shared scope prefix in titles.
///
/// Titles like "Auth: fix login" and "Auth: add 2FA" share the "Auth" scope.
/// Also handles bracket prefixes like "[graphfs.go] Replace..." and "[graphfs.go] Add...".
fn scope_prefix_similarity(a: &str, b: &str) -> f64 {
    // Try colon-delimited prefix
    if let (Some(pa), Some(pb)) = (a.split_once(':'), b.split_once(':'))
        && pa.0.trim().eq_ignore_ascii_case(pb.0.trim())
        && !pa.0.trim().is_empty()
    {
        return 1.0;
    }

    // Try bracket prefix: [foo] bar
    if let (Some(pa), Some(pb)) = (bracket_prefix(a), bracket_prefix(b))
        && pa.eq_ignore_ascii_case(pb)
    {
        return 1.0;
    }

    0.0
}

/// Extract a bracket prefix like "[graphfs.go]" → "graphfs.go".
fn bracket_prefix(s: &str) -> Option<&str> {
    let s = s.trim();
    if s.starts_with('[') {
        s.find(']').map(|i| &s[1..i])
    } else {
        None
    }
}

/// Detect sequential/phase patterns in titles.
///
/// Matches patterns like:
///   "step 1: foo" / "step 2: bar"
///   "phase A: foo" / "phase B: bar"
///   "part 1 — foo" / "part 2 — bar"
///   "foo (1/3)" / "foo (2/3)"
fn sequential_similarity(a: &str, b: &str) -> f64 {
    let a_lower = a.to_lowercase();
    let b_lower = b.to_lowercase();

    // Strip numbering tokens and compare the rest
    let a_stripped = strip_sequence_tokens(&a_lower);
    let b_stripped = strip_sequence_tokens(&b_lower);

    if a_stripped == b_stripped && !a_stripped.is_empty() && a_lower != b_lower {
        // Same base text, different numbering → sequential
        return 1.0;
    }

    // Check for shared base with N/M pattern: "foo (1/3)" vs "foo (2/3)"
    let a_base = strip_n_of_m(&a_lower);
    let b_base = strip_n_of_m(&b_lower);
    if a_base == b_base && !a_base.is_empty() && a_lower != b_lower {
        return 1.0;
    }

    0.0
}

/// Strip common sequence tokens: "step 1", "phase a", "part 2", leading numbers.
fn strip_sequence_tokens(s: &str) -> String {
    let s = s.trim();
    // Remove leading "step N", "phase N", "part N"
    let prefixes = ["step", "phase", "part", "stage"];
    for prefix in &prefixes {
        if let Some(rest) = s.strip_prefix(prefix) {
            let rest = rest.trim().trim_start_matches(|c: char| {
                c.is_ascii_digit()
                    || c == '.'
                    || c == ':'
                    || c == '-'
                    || c == '–'
                    || c == '—'
                    || c == ' '
                    || c.is_alphabetic() && rest.trim().len() <= 2
            });
            // Only strip if we actually found a sequence marker (digit/letter after prefix)
            let trimmed = rest.trim_start_matches([':', '-', '–', '—', ' ']);
            if trimmed.len() < s.len() {
                return trimmed.to_string();
            }
        }
    }

    // Remove trailing "(N/M)" or "(N)"
    strip_n_of_m(s)
}

/// Remove trailing "(N/M)" or "(N)" patterns.
fn strip_n_of_m(s: &str) -> String {
    let s = s.trim();
    if let Some(idx) = s.rfind('(') {
        let candidate = &s[idx..];
        if candidate.ends_with(')')
            && candidate[1..candidate.len() - 1]
                .chars()
                .all(|c| c.is_ascii_digit() || c == '/')
        {
            return s[..idx].trim().to_string();
        }
    }
    s.to_string()
}

// ---------------------------------------------------------------------------
// Union-Find clustering
// ---------------------------------------------------------------------------

struct UnionFind {
    parent: Vec<usize>,
    rank: Vec<usize>,
}

impl UnionFind {
    fn new(n: usize) -> Self {
        UnionFind {
            parent: (0..n).collect(),
            rank: vec![0; n],
        }
    }

    fn find(&mut self, x: usize) -> usize {
        if self.parent[x] != x {
            self.parent[x] = self.find(self.parent[x]);
        }
        self.parent[x]
    }

    fn union(&mut self, x: usize, y: usize) {
        let rx = self.find(x);
        let ry = self.find(y);
        if rx == ry {
            return;
        }
        match self.rank[rx].cmp(&self.rank[ry]) {
            std::cmp::Ordering::Less => self.parent[rx] = ry,
            std::cmp::Ordering::Greater => self.parent[ry] = rx,
            std::cmp::Ordering::Equal => {
                self.parent[ry] = rx;
                self.rank[rx] += 1;
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/// Minimum combined similarity to consider two beads related.
const CLUSTER_THRESHOLD: f64 = 0.45;

/// Cluster a set of beads by semantic similarity.
///
/// Returns a list of clusters, each containing 2+ related beads. Singletons are
/// not returned. The caller (reconciler) uses clusters to suppress duplicate
/// dispatch and order sequential work.
pub fn cluster_beads(beads: &[Bead]) -> Vec<BeadCluster> {
    if beads.len() < 2 {
        return Vec::new();
    }

    // Compute pairwise similarity and build union-find
    let n = beads.len();
    let mut uf = UnionFind::new(n);
    let mut pair_scores: HashMap<(usize, usize), f64> = HashMap::new();

    for i in 0..n {
        for j in (i + 1)..n {
            // Only cluster within the same repo
            if beads[i].repo != beads[j].repo {
                continue;
            }
            let sim = combined_similarity(&beads[i], &beads[j]);
            if sim >= CLUSTER_THRESHOLD {
                uf.union(i, j);
                pair_scores.insert((i, j), sim);
            }
        }
    }

    // Collect clusters
    let mut groups: HashMap<usize, Vec<usize>> = HashMap::new();
    for i in 0..n {
        let root = uf.find(i);
        groups.entry(root).or_default().push(i);
    }

    groups
        .into_values()
        .filter(|members| members.len() >= 2)
        .map(|members| build_cluster(beads, &members, &pair_scores))
        .collect()
}

/// Check if a bead is dominated by (semantically covered by) any bead in a set.
///
/// This is the reconciler integration point: replaces the simple Jaccard check.
/// Returns the ID of the dominating bead, if any.
pub fn is_dominated_by(bead: &Bead, active_beads: &[&Bead]) -> Option<String> {
    for other in active_beads {
        if other.id == bead.id {
            continue;
        }
        if bead.repo != other.repo {
            continue;
        }
        let sim = combined_similarity(bead, other);
        if sim > 0.5 {
            return Some(other.id.clone());
        }
    }
    None
}

/// Check if two scope paths overlap.
///
/// Supports both files and directories:
/// - File `src/dolt.rs` overlaps file `src/dolt.rs` (exact match)
/// - Directory `src/` overlaps file `src/dolt.rs` (prefix match)
/// - Directory `crates/bdr/` overlaps `crates/bdr/src/harmony.rs`
/// - Directory `src/` overlaps directory `src/dispatch/` (nested)
///
/// Convention: directories end with `/`, files don't.
fn scopes_overlap(a: &str, b: &str) -> bool {
    let a = a.strip_prefix("./").unwrap_or(a);
    let b = b.strip_prefix("./").unwrap_or(b);
    if a == b {
        return true;
    }
    // Directory prefix: "src/" matches "src/dolt.rs"
    if a.ends_with('/') && b.starts_with(a) {
        return true;
    }
    if b.ends_with('/') && a.starts_with(b) {
        return true;
    }
    false
}

/// Check if a candidate bead's files overlap with any active/queued bead.
///
/// Returns the ID of the conflicting bead if scopes intersect.
/// Supports both file paths (`src/dolt.rs`) and directory scopes (`crates/bdr/`).
/// Beads without scopes are always allowed — overlap is only detected
/// when **both** beads have scopes set.
///
/// Paths are normalized by stripping leading `./` before comparison.
pub fn has_file_overlap(bead: &Bead, active_beads: &[&Bead]) -> Option<String> {
    if bead.files.is_empty() && bead.test_files.is_empty() {
        return None;
    }

    let candidate_scopes: Vec<&str> = bead
        .files
        .iter()
        .chain(bead.test_files.iter())
        .map(|f| f.strip_prefix("./").unwrap_or(f.as_str()))
        .collect();

    for other in active_beads {
        if other.id == bead.id || other.repo != bead.repo {
            continue;
        }
        if other.files.is_empty() && other.test_files.is_empty() {
            continue;
        }

        let other_scopes: Vec<&str> = other
            .files
            .iter()
            .chain(other.test_files.iter())
            .map(|f| f.strip_prefix("./").unwrap_or(f.as_str()))
            .collect();

        for cs in &candidate_scopes {
            for os in &other_scopes {
                if scopes_overlap(cs, os) {
                    return Some(other.id.clone());
                }
            }
        }
    }
    None
}

/// Suggest a priority adjustment for a cluster member based on its cluster.
///
/// If the cluster contains a high-priority bead, related lower-priority beads
/// should be boosted (their work is part of the same initiative).
pub fn suggest_priority(bead: &Bead, cluster: &BeadCluster, all_beads: &[Bead]) -> Option<u8> {
    let min_priority = cluster
        .bead_ids
        .iter()
        .filter_map(|id| all_beads.iter().find(|b| b.id == *id))
        .map(|b| b.priority)
        .min()?;

    // Only suggest if bead is 2+ levels lower than cluster's best
    if bead.priority > min_priority + 1 {
        Some(min_priority + 1)
    } else {
        None
    }
}

// ---------------------------------------------------------------------------
// Cluster construction
// ---------------------------------------------------------------------------

fn build_cluster(
    beads: &[Bead],
    members: &[usize],
    pair_scores: &HashMap<(usize, usize), f64>,
) -> BeadCluster {
    // Compute average cohesion
    let mut total_sim = 0.0;
    let mut pair_count = 0;
    for (i, &mi) in members.iter().enumerate() {
        for &mj in &members[i + 1..] {
            let key = if mi < mj { (mi, mj) } else { (mj, mi) };
            if let Some(&score) = pair_scores.get(&key) {
                total_sim += score;
                pair_count += 1;
            }
        }
    }
    let cohesion = if pair_count > 0 {
        total_sim / pair_count as f64
    } else {
        0.0
    };

    // Determine relationship type
    let relationship = classify_relationship(beads, members);

    // Order by priority then creation time
    let mut ordered: Vec<usize> = members.to_vec();
    ordered.sort_by(|&a, &b| {
        beads[a]
            .priority
            .cmp(&beads[b].priority)
            .then(beads[a].created_at.cmp(&beads[b].created_at))
    });

    let bead_ids: Vec<String> = ordered.iter().map(|&i| beads[i].id.clone()).collect();

    // Determine action
    let action = match &relationship {
        ClusterRelationship::NearDuplicate => {
            let keep = bead_ids[0].clone(); // highest priority, oldest
            let close = bead_ids[1..].to_vec();
            ClusterAction::Merge { keep, close }
        }
        ClusterRelationship::Sequential => ClusterAction::Sequence,
        _ => ClusterAction::Suppress,
    };

    BeadCluster {
        bead_ids,
        relationship,
        action,
        cohesion,
    }
}

fn classify_relationship(beads: &[Bead], members: &[usize]) -> ClusterRelationship {
    // Check for near-duplicates first (highest Jaccard)
    let all_near_dup = members.iter().all(|&i| {
        members
            .iter()
            .filter(|&&j| j != i)
            .all(|&j| jaccard_similarity(&beads[i].title, &beads[j].title) > 0.8)
    });
    if all_near_dup {
        return ClusterRelationship::NearDuplicate;
    }

    // Check for sequential pattern
    let any_sequential = members.iter().any(|&i| {
        members
            .iter()
            .filter(|&&j| j != i)
            .any(|&j| sequential_similarity(&beads[i].title, &beads[j].title) > 0.5)
    });
    if any_sequential {
        return ClusterRelationship::Sequential;
    }

    // Check for shared scope
    let any_scoped = members.iter().any(|&i| {
        members
            .iter()
            .filter(|&&j| j != i)
            .any(|&j| scope_prefix_similarity(&beads[i].title, &beads[j].title) > 0.5)
    });
    if any_scoped {
        return ClusterRelationship::SharedScope;
    }

    ClusterRelationship::Overlapping
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Utc;

    fn make_bead(id: &str, title: &str) -> Bead {
        Bead {
            id: id.to_string(),
            title: title.to_string(),
            description: String::new(),
            status: "open".to_string(),
            priority: 2,
            issue_type: "task".to_string(),
            owner: None,
            repo: "test".to_string(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            dependency_count: 0,
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

    fn make_bead_full(id: &str, title: &str, desc: &str, priority: u8, repo: &str) -> Bead {
        Bead {
            id: id.to_string(),
            title: title.to_string(),
            description: desc.to_string(),
            priority,
            repo: repo.to_string(),
            ..make_bead(id, title)
        }
    }

    // --- scope prefix ---

    #[test]
    fn scope_prefix_colon() {
        assert_eq!(
            scope_prefix_similarity("Auth: fix login", "Auth: add 2FA"),
            1.0
        );
    }

    #[test]
    fn scope_prefix_bracket() {
        assert_eq!(
            scope_prefix_similarity(
                "[graphfs.go] Replace interface{}",
                "[graphfs.go] Add method"
            ),
            1.0
        );
    }

    #[test]
    fn scope_prefix_none() {
        assert_eq!(scope_prefix_similarity("fix login", "add 2FA"), 0.0);
    }

    // --- sequential ---

    #[test]
    fn sequential_n_of_m() {
        assert_eq!(
            sequential_similarity("migrate users (1/3)", "migrate users (2/3)"),
            1.0
        );
    }

    #[test]
    fn sequential_not_related() {
        assert_eq!(
            sequential_similarity("fix login bug", "add new dashboard"),
            0.0
        );
    }

    // --- combined similarity ---

    #[test]
    fn combined_near_duplicate() {
        let a = make_bead("a", "fix the widget rendering bug");
        let b = make_bead("b", "fix the widget rendering bug in dark mode");
        let sim = combined_similarity(&a, &b);
        assert!(
            sim > 0.4,
            "near-duplicate titles should have high sim: {sim}"
        );
    }

    #[test]
    fn combined_unrelated() {
        let a = make_bead("a", "fix login page CSS");
        let b = make_bead("b", "add payment processing endpoint");
        let sim = combined_similarity(&a, &b);
        assert!(
            sim < CLUSTER_THRESHOLD,
            "unrelated beads should have low sim: {sim}"
        );
    }

    #[test]
    fn combined_shared_scope() {
        let a = make_bead("a", "Auth: fix login");
        let b = make_bead("b", "Auth: add 2FA support");
        let sim = combined_similarity(&a, &b);
        assert!(
            sim >= CLUSTER_THRESHOLD,
            "shared scope should cluster: {sim}"
        );
    }

    // --- jaccard_filtered ---

    #[test]
    fn jaccard_filtered_strips_common_verbs() {
        // These titles share only refactoring verbs — should NOT be similar
        let sim = jaccard_filtered("consolidate skip-dir handling", "extract toNodeID helper");
        assert!(
            sim < 0.2,
            "titles sharing only stopwords should have near-zero sim: {sim}"
        );
    }

    #[test]
    fn jaccard_filtered_preserves_real_overlap() {
        // These titles share meaningful tokens — should still be similar
        let sim = jaccard_filtered(
            "fix widget rendering bug",
            "fix widget rendering in dark mode",
        );
        assert!(
            sim > 0.3,
            "titles with real content overlap should remain similar: {sim}"
        );
    }

    #[test]
    fn jaccard_filtered_all_stopwords_returns_zero() {
        let sim = jaccard_filtered("fix add replace", "fix extract implement");
        assert_eq!(sim, 0.0, "all-stopword titles should return 0.0");
    }

    // --- false-positive regression ---

    #[test]
    fn same_file_refactoring_beads_not_dominated() {
        // Regression: beads referencing the same file with different refactoring
        // work were incorrectly marked as duplicates because common verbs
        // inflated Jaccard similarity past the 0.5 threshold.
        let a = make_bead("a", "consolidate skip-dir handling in engine.go");
        let b = make_bead("b", "extract toNodeID helper from engine.go");
        let c = make_bead(
            "c",
            "replace linear scan with map lookup in engine.go dedup",
        );

        assert!(
            is_dominated_by(&b, &[&a]).is_none(),
            "extract toNodeID should not be dominated by consolidate skip-dir"
        );
        assert!(
            is_dominated_by(&c, &[&a]).is_none(),
            "linear scan dedup should not be dominated by consolidate skip-dir"
        );
        assert!(
            is_dominated_by(&c, &[&b]).is_none(),
            "linear scan dedup should not be dominated by extract toNodeID"
        );
    }

    // --- clustering ---

    #[test]
    fn cluster_near_duplicates() {
        let beads = vec![
            make_bead("a", "fix the widget bug"),
            make_bead("b", "fix the widget bug in production"),
            make_bead("c", "add new dashboard feature"),
        ];
        let clusters = cluster_beads(&beads);
        // a and b should cluster; c is unrelated
        assert_eq!(clusters.len(), 1);
        assert!(clusters[0].bead_ids.contains(&"a".to_string()));
        assert!(clusters[0].bead_ids.contains(&"b".to_string()));
        assert!(!clusters[0].bead_ids.contains(&"c".to_string()));
    }

    #[test]
    fn cluster_shared_scope() {
        let beads = vec![
            make_bead(
                "a",
                "[scanner.rs] Replace unwrap with proper error handling",
            ),
            make_bead("b", "[scanner.rs] Add unit tests for edge cases"),
            make_bead("c", "[dolt.rs] Fix connection timeout"),
        ];
        let clusters = cluster_beads(&beads);
        assert_eq!(clusters.len(), 1);
        assert!(clusters[0].bead_ids.contains(&"a".to_string()));
        assert!(clusters[0].bead_ids.contains(&"b".to_string()));
        assert_eq!(clusters[0].relationship, ClusterRelationship::SharedScope);
    }

    #[test]
    fn cluster_cross_repo_not_grouped() {
        let beads = vec![
            make_bead_full("a", "fix the widget bug", "", 2, "repo-a"),
            make_bead_full("b", "fix the widget bug", "", 2, "repo-b"),
        ];
        let clusters = cluster_beads(&beads);
        assert!(clusters.is_empty(), "cross-repo beads should not cluster");
    }

    #[test]
    fn cluster_empty_input() {
        assert!(cluster_beads(&[]).is_empty());
    }

    #[test]
    fn cluster_singleton() {
        let beads = vec![make_bead("a", "only one bead")];
        assert!(cluster_beads(&beads).is_empty());
    }

    // --- is_dominated_by ---

    #[test]
    fn dominated_by_similar_active() {
        let active = make_bead("a", "fix the widget rendering bug");
        let candidate = make_bead("b", "fix the widget rendering bug in dark mode");
        let result = is_dominated_by(&candidate, &[&active]);
        assert_eq!(result, Some("a".to_string()));
    }

    #[test]
    fn not_dominated_by_unrelated() {
        let active = make_bead("a", "fix login page CSS");
        let candidate = make_bead("b", "add payment processing endpoint");
        let result = is_dominated_by(&candidate, &[&active]);
        assert!(result.is_none());
    }

    #[test]
    fn dominated_respects_repo_boundary() {
        let active = make_bead_full("a", "fix the widget bug", "", 2, "repo-a");
        let candidate = make_bead_full("b", "fix the widget bug", "", 2, "repo-b");
        let result = is_dominated_by(&candidate, &[&active]);
        assert!(result.is_none(), "cross-repo should not dominate");
    }

    // --- suggest_priority ---

    #[test]
    fn suggest_priority_boost() {
        let cluster = BeadCluster {
            bead_ids: vec!["a".into(), "b".into()],
            relationship: ClusterRelationship::SharedScope,
            action: ClusterAction::Suppress,
            cohesion: 0.6,
        };
        let beads = vec![
            make_bead_full("a", "Auth: fix login", "", 0, "test"),
            make_bead_full("b", "Auth: add 2FA", "", 3, "test"),
        ];
        let suggestion = suggest_priority(&beads[1], &cluster, &beads);
        assert_eq!(
            suggestion,
            Some(1),
            "P3 bead in P0 cluster should be boosted to P1"
        );
    }

    #[test]
    fn suggest_priority_no_change() {
        let cluster = BeadCluster {
            bead_ids: vec!["a".into(), "b".into()],
            relationship: ClusterRelationship::SharedScope,
            action: ClusterAction::Suppress,
            cohesion: 0.6,
        };
        let beads = vec![
            make_bead_full("a", "Auth: fix login", "", 1, "test"),
            make_bead_full("b", "Auth: add 2FA", "", 2, "test"),
        ];
        let suggestion = suggest_priority(&beads[1], &cluster, &beads);
        assert!(suggestion.is_none(), "P2 in P1 cluster needs no boost");
    }

    // --- merge action ---

    #[test]
    fn merge_action_keeps_highest_priority() {
        let beads = vec![
            make_bead_full("a", "fix widget bug", "", 1, "test"),
            make_bead_full("b", "fix widget bug in production", "", 2, "test"),
        ];
        let clusters = cluster_beads(&beads);
        assert_eq!(clusters.len(), 1);
        if let ClusterAction::Merge {
            ref keep,
            ref close,
        } = clusters[0].action
        {
            assert_eq!(keep, "a", "should keep higher-priority bead");
            assert_eq!(close, &vec!["b".to_string()]);
        }
        // NearDuplicate is expected for very similar titles
    }

    // --- file overlap ---

    fn make_bead_with_files(id: &str, title: &str, files: &[&str], test_files: &[&str]) -> Bead {
        Bead {
            files: files.iter().map(|f| f.to_string()).collect(),
            test_files: test_files.iter().map(|f| f.to_string()).collect(),
            ..make_bead(id, title)
        }
    }

    #[test]
    fn file_overlap_detected() {
        let active = make_bead_with_files("a", "fix serve", &["src/serve.rs"], &[]);
        let candidate =
            make_bead_with_files("b", "add endpoint", &["src/serve.rs", "src/api.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert_eq!(
            result,
            Some("a".to_string()),
            "overlapping src/serve.rs should conflict"
        );
    }

    #[test]
    fn file_overlap_disjoint_ok() {
        let active = make_bead_with_files("a", "fix dolt", &["src/dolt.rs"], &[]);
        let candidate = make_bead_with_files("b", "fix dispatch", &["src/dispatch.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(result.is_none(), "disjoint files should not conflict");
    }

    #[test]
    fn file_overlap_no_files_always_ok() {
        let active = make_bead_with_files("a", "fix serve", &["src/serve.rs"], &[]);
        let candidate = make_bead("b", "vague task");
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(
            result.is_none(),
            "candidate without files should always pass"
        );
    }

    #[test]
    fn file_overlap_active_no_files_ok() {
        let active = make_bead("a", "vague active task");
        let candidate = make_bead_with_files("b", "fix serve", &["src/serve.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(
            result.is_none(),
            "active bead without files should not block"
        );
    }

    #[test]
    fn file_overlap_test_files_conflict() {
        let active =
            make_bead_with_files("a", "fix dolt", &["src/dolt.rs"], &["tests/dolt_test.rs"]);
        let candidate =
            make_bead_with_files("b", "dolt perf", &["src/pool.rs"], &["tests/dolt_test.rs"]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert_eq!(
            result,
            Some("a".to_string()),
            "overlapping test_files should conflict"
        );
    }

    #[test]
    fn file_overlap_normalizes_dot_slash() {
        let active = make_bead_with_files("a", "fix serve", &["./src/serve.rs"], &[]);
        let candidate = make_bead_with_files("b", "add endpoint", &["src/serve.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert_eq!(
            result,
            Some("a".to_string()),
            "./src/serve.rs and src/serve.rs should match"
        );
    }

    #[test]
    fn file_overlap_cross_repo_ignored() {
        let active = make_bead_with_files("a", "fix serve", &["src/serve.rs"], &[]);
        let mut candidate = make_bead_with_files("b", "fix serve", &["src/serve.rs"], &[]);
        candidate.repo = "other-repo".to_string();
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(
            result.is_none(),
            "cross-repo file overlap should not conflict"
        );
    }

    // --- directory scope overlap ---

    #[test]
    fn dir_scope_overlaps_file() {
        let active = make_bead_with_files("a", "design bdr", &["crates/bdr/"], &[]);
        let candidate =
            make_bead_with_files("b", "fix harmony", &["crates/bdr/src/harmony.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert_eq!(
            result,
            Some("a".to_string()),
            "directory scope crates/bdr/ must overlap crates/bdr/src/harmony.rs"
        );
    }

    #[test]
    fn dir_scope_overlaps_nested_dir() {
        let active = make_bead_with_files("a", "src redesign", &["src/"], &[]);
        let candidate = make_bead_with_files("b", "dispatch work", &["src/dispatch/"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert_eq!(
            result,
            Some("a".to_string()),
            "parent dir src/ must overlap nested src/dispatch/"
        );
    }

    #[test]
    fn dir_scope_no_overlap_sibling() {
        let active = make_bead_with_files("a", "bdr design", &["crates/bdr/"], &[]);
        let candidate = make_bead_with_files("b", "conductor work", &["conductor/"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(
            result.is_none(),
            "sibling directories crates/bdr/ and conductor/ must not overlap"
        );
    }

    #[test]
    fn file_does_not_overlap_similar_prefix() {
        let active = make_bead_with_files("a", "fix serve", &["src/serve.rs"], &[]);
        let candidate = make_bead_with_files("b", "fix serve test", &["src/serve_test.rs"], &[]);
        let result = has_file_overlap(&candidate, &[&active]);
        assert!(
            result.is_none(),
            "src/serve.rs and src/serve_test.rs are different files, not a prefix match"
        );
    }

    #[test]
    fn scopes_overlap_unit() {
        // Exact match
        assert!(scopes_overlap("src/dolt.rs", "src/dolt.rs"));
        // Dir overlaps file
        assert!(scopes_overlap("src/", "src/dolt.rs"));
        assert!(scopes_overlap("src/dolt.rs", "src/"));
        // Nested dirs
        assert!(scopes_overlap("crates/", "crates/bdr/src/lib.rs"));
        // No overlap
        assert!(!scopes_overlap("src/dolt.rs", "src/serve.rs"));
        assert!(!scopes_overlap("src/", "conductor/"));
        // Dot-slash normalization
        assert!(scopes_overlap("./src/dolt.rs", "src/dolt.rs"));
    }

    // --- strip_n_of_m ---

    #[test]
    fn strip_n_of_m_removes_pattern() {
        assert_eq!(strip_n_of_m("migrate users (2/3)"), "migrate users");
        assert_eq!(strip_n_of_m("no pattern here"), "no pattern here");
    }
}
