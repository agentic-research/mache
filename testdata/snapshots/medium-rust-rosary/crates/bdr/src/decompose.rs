// Atom → bead mapper with channel annotations

use crate::atom::{Atom, AtomKind};
use crate::channels::BdrChannel;
use crate::parse::DocMeta;
use crate::provenance::{InferenceTrace, ProvenanceRef};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

/// A bead ready to be created, annotated with BDR lattice metadata.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BeadSpec {
    pub title: String,
    pub description: String,
    pub issue_type: String,
    pub priority: u8,
    pub channel: BdrChannel,
    pub thread_group: String,
    pub source_atom: AtomKind,
    /// Ordered provenance chain — earlier entries were processed first.
    /// First entry is the primary source; subsequent entries are secondary
    /// docs pulled in during parsing (cross-references, depends_on ADRs).
    pub derived_from: Vec<ProvenanceRef>,
    pub source_line: usize,
    pub references: Vec<String>,
    /// Target repo for this bead (from doc frontmatter or reference analysis).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target_repo: Option<String>,
    /// ADR IDs this bead depends on (from frontmatter `Depends on:`).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub depends_on: Vec<String>,
    /// Structured success criteria extracted from ValidationPoint atoms.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub success_criteria: Vec<SuccessCriterion>,
    /// Set when the LLM classifier was invoked to resolve ambiguous sections.
    /// Always posterior to `derived_from`. Excluded from content_hash.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub inferred_from: Option<InferenceTrace>,
    /// Minimum required doc-coverage fraction (0.0–1.0) for the verify gate.
    ///
    /// When set, the assay plugin in the verify pipeline must report
    /// `coverage >= doc_coverage_min` or the bead's verify tier fails.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub doc_coverage_min: Option<f32>,
}

/// A verifiable success criterion for a bead.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SuccessCriterion {
    /// Human-readable description of the criterion.
    pub description: String,
    /// Optional verification command (e.g., "cargo test", "task lint").
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub command: Option<String>,
    /// Quantitative threshold if applicable (e.g., ">90% hit rate").
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub threshold: Option<String>,
}

impl BeadSpec {
    /// Content hash of the immutable bead definition.
    ///
    /// Covers: title, description, issue_type, priority, success_criteria.
    /// Does NOT cover provenance (derived_from, inferred_from), routing
    /// (target_repo, depends_on), or mutable state — those are metadata.
    ///
    /// Properties:
    /// - Same doc decomposition → same hash (dedup across runs)
    /// - Dependency references use content_hash, not string IDs (tamper-evident)
    /// - Federation: push content hashes, receiver verifies locally
    pub fn content_hash(&self) -> [u8; 32] {
        use sha2::{Digest, Sha256};
        let mut hasher = Sha256::new();
        hasher.update(self.title.as_bytes());
        hasher.update(b"\0");
        hasher.update(self.description.as_bytes());
        hasher.update(b"\0");
        hasher.update(self.issue_type.as_bytes());
        hasher.update(b"\0");
        hasher.update([self.priority]);
        for sc in &self.success_criteria {
            hasher.update(sc.description.as_bytes());
            if let Some(ref cmd) = sc.command {
                hasher.update(cmd.as_bytes());
            }
            if let Some(ref threshold) = sc.threshold {
                hasher.update(threshold.as_bytes());
            }
            hasher.update(b"\0");
        }
        hasher.finalize().into()
    }

    /// Hex-encoded content hash (for display and storage).
    pub fn content_hash_hex(&self) -> String {
        hex::encode(self.content_hash())
    }

    /// Convenience: the primary source (first entry in derived_from).
    pub fn primary_source(&self) -> Option<&ProvenanceRef> {
        self.derived_from.first()
    }
}

/// Channel assignment for each atom kind.
pub fn channel_for_atom(kind: AtomKind) -> BdrChannel {
    match kind {
        AtomKind::FrictionPoint => BdrChannel::Decade,
        AtomKind::Constraint => BdrChannel::Decade,
        AtomKind::Alternative => BdrChannel::Decade,
        AtomKind::Decision => BdrChannel::Thread,
        AtomKind::Consequence => BdrChannel::Thread,
        AtomKind::OpenQuestion => BdrChannel::Thread,
        AtomKind::Phase => BdrChannel::Thread,
        AtomKind::ValidationPoint => BdrChannel::Bead,
        AtomKind::TechnicalSpec => BdrChannel::Bead,
    }
}

/// Decompose a list of atoms into bead specs (backward-compatible, no meta).
pub fn decompose(atoms: &[Atom], doc_id: &str) -> Vec<BeadSpec> {
    decompose_with_meta(atoms, doc_id, &DocMeta::default())
}

/// Decompose a list of atoms into bead specs with document metadata.
pub fn decompose_with_meta(atoms: &[Atom], doc_id: &str, meta: &DocMeta) -> Vec<BeadSpec> {
    let primary_source = meta
        .provenance
        .clone()
        .unwrap_or_else(|| ProvenanceRef::Adr {
            id: doc_id.to_string(),
        });

    atoms
        .iter()
        .map(|atom| {
            let channel = channel_for_atom(atom.kind);
            let thread_group = assign_thread_group(atom);
            let target_repo = infer_target_repo(atom, meta);
            let success_criteria = extract_success_criteria(atom);

            BeadSpec {
                title: format!("[{}] {}", doc_id, truncate(&atom.title, 60)),
                description: atom.body.clone(),
                issue_type: atom.kind.suggested_issue_type().to_string(),
                priority: atom.kind.suggested_priority(),
                channel,
                thread_group,
                source_atom: atom.kind,
                derived_from: vec![primary_source.clone()],
                source_line: atom.source_line,
                references: atom.references.clone(),
                target_repo,
                depends_on: meta.depends_on.clone(),
                success_criteria,
                inferred_from: None,
                doc_coverage_min: None,
            }
        })
        .collect()
}

/// Infer which repo a bead should be created in.
/// Priority: atom references mentioning specific repos > doc-level repo field.
fn infer_target_repo(atom: &Atom, meta: &DocMeta) -> Option<String> {
    // Check atom references for repo-like patterns
    // Patterns: "repo:something", "crates/name/", "repo/path"
    for r in &atom.references {
        if let Some((repo, _)) = r.split_once(':') {
            let repo = repo.trim();
            if !repo.is_empty() && !repo.contains("//") && !repo.contains(' ') && repo.len() < 30 {
                return Some(repo.to_string());
            }
        }
    }
    // Fall back to doc-level repo
    meta.repo.clone()
}

/// Extract structured success criteria from ValidationPoint atoms.
/// Parses list items and attempts to identify commands and thresholds.
fn extract_success_criteria(atom: &Atom) -> Vec<SuccessCriterion> {
    if atom.kind != AtomKind::ValidationPoint {
        return Vec::new();
    }

    let mut criteria = Vec::new();

    // Parse the body for individual criteria
    // Each list item or sentence is a criterion
    for line in atom.body.lines() {
        let trimmed = line.trim();
        let text = trimmed
            .strip_prefix("- ")
            .or_else(|| trimmed.strip_prefix("* "))
            .unwrap_or(trimmed);

        if text.is_empty() {
            continue;
        }

        let command = extract_command(text);
        let threshold = extract_threshold(text);

        criteria.push(SuccessCriterion {
            description: text.to_string(),
            command,
            threshold,
        });
    }

    // If no list items, treat whole body as single criterion
    if criteria.is_empty() && !atom.body.trim().is_empty() {
        criteria.push(SuccessCriterion {
            description: atom.body.trim().to_string(),
            command: extract_command(&atom.body),
            threshold: extract_threshold(&atom.body),
        });
    }

    criteria
}

/// Extract a command from text (backtick-wrapped commands like `cargo test`).
fn extract_command(text: &str) -> Option<String> {
    // Find backtick-wrapped content that looks like a command
    let parts: Vec<&str> = text.split('`').collect();
    for chunk in parts.chunks(2) {
        if chunk.len() == 2 {
            let candidate = chunk[1].trim();
            if looks_like_command(candidate) {
                return Some(candidate.to_string());
            }
        }
    }
    None
}

/// Check if a backtick-wrapped string looks like a shell command.
fn looks_like_command(s: &str) -> bool {
    let cmd_prefixes = [
        "cargo", "task", "go", "npm", "make", "mix", "elixir", "rustc", "gcc", "python", "pytest",
        "rg", "grep", "curl", "jj", "git",
    ];
    cmd_prefixes
        .iter()
        .any(|p| s.starts_with(p) || s.starts_with(&format!("./{p}")))
}

/// Extract a quantitative threshold from text (percentages, comparisons).
fn extract_threshold(text: &str) -> Option<String> {
    // Look for patterns like ">90%", "<100ms", "≥95%", "< 3 hops"
    let words: Vec<&str> = text.split_whitespace().collect();
    for (i, word) in words.iter().enumerate() {
        if word.ends_with('%') || word.contains("ms") || word.contains("MB") || word.contains("KB")
        {
            // Include the comparison operator if present
            if i > 0 && ["<", ">", "<=", ">=", "≤", "≥", "~"].contains(&words[i - 1]) {
                return Some(format!("{} {}", words[i - 1], word));
            }
            return Some(word.to_string());
        }
    }
    None
}

/// Group bead specs by thread_group.
pub fn group_by_thread(specs: &[BeadSpec]) -> BTreeMap<String, Vec<&BeadSpec>> {
    let mut groups: BTreeMap<String, Vec<&BeadSpec>> = BTreeMap::new();
    for spec in specs {
        groups
            .entry(spec.thread_group.clone())
            .or_default()
            .push(spec);
    }
    groups
}

/// Assign a thread grouping key based on section name and atom kind.
fn assign_thread_group(atom: &Atom) -> String {
    let section = atom.source_section.to_lowercase();

    if section.contains("context") || section.contains("problem") {
        "context".to_string()
    } else if section.contains("decision") {
        "decision".to_string()
    } else if section.contains("consequence") || section.contains("impact") {
        "consequences".to_string()
    } else if section.contains("implementation") || section.contains("phase") {
        "implementation".to_string()
    } else if section.contains("validation") || section.contains("metric") {
        "validation".to_string()
    } else if section.contains("question") || section.contains("unknown") {
        "open-questions".to_string()
    } else if section.contains("alternative") {
        "alternatives".to_string()
    } else {
        "general".to_string()
    }
}

/// Truncate a string to max_len, appending "..." if truncated.
fn truncate(s: &str, max_len: usize) -> String {
    if s.len() <= max_len {
        s.to_string()
    } else {
        format!("{}...", &s[..max_len.saturating_sub(3)])
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::atom::ALL_ATOM_KINDS;

    fn sample_atom(kind: AtomKind) -> Atom {
        Atom {
            kind,
            title: "Test atom".to_string(),
            body: "Test body with `ref-1` details".to_string(),
            source_line: 10,
            source_section: "Context".to_string(),
            references: vec!["ref-1".to_string()],
        }
    }

    fn adr_source(id: &str) -> ProvenanceRef {
        ProvenanceRef::Adr { id: id.to_string() }
    }

    #[test]
    fn friction_point_maps_to_decade() {
        assert_eq!(
            channel_for_atom(AtomKind::FrictionPoint),
            BdrChannel::Decade
        );
    }

    #[test]
    fn decision_maps_to_thread() {
        assert_eq!(channel_for_atom(AtomKind::Decision), BdrChannel::Thread);
    }

    #[test]
    fn validation_point_maps_to_bead() {
        assert_eq!(
            channel_for_atom(AtomKind::ValidationPoint),
            BdrChannel::Bead
        );
    }

    #[test]
    fn all_atom_kinds_have_channel_mapping() {
        for kind in ALL_ATOM_KINDS {
            let _ = channel_for_atom(kind); // should not panic
        }
    }

    #[test]
    fn decompose_single_atom() {
        let atoms = vec![sample_atom(AtomKind::FrictionPoint)];
        let specs = decompose(&atoms, "ADR-001");

        assert_eq!(specs.len(), 1);
        assert_eq!(specs[0].issue_type, "task");
        assert_eq!(specs[0].channel, BdrChannel::Decade);
        assert_eq!(specs[0].derived_from, vec![adr_source("ADR-001")]);
        assert!(specs[0].title.starts_with("[ADR-001]"));
    }

    #[test]
    fn decompose_preserves_references() {
        let atoms = vec![sample_atom(AtomKind::Decision)];
        let specs = decompose(&atoms, "ADR-001");
        assert_eq!(specs[0].references, vec!["ref-1".to_string()]);
    }

    #[test]
    fn decompose_empty_atoms() {
        let specs = decompose(&[], "ADR-001");
        assert!(specs.is_empty());
    }

    #[test]
    fn phase_maps_to_epic() {
        let atom = Atom {
            kind: AtomKind::Phase,
            title: "Phase 1: Scaffold".into(),
            body: "Create crate".into(),
            source_line: 1,
            source_section: "Implementation Plan".into(),
            references: vec![],
        };
        let specs = decompose(&[atom], "ADR-001");
        assert_eq!(specs[0].issue_type, "epic");
        assert_eq!(specs[0].channel, BdrChannel::Thread);
        assert_eq!(specs[0].thread_group, "implementation");
    }

    #[test]
    fn group_by_thread_clusters() {
        let atoms = vec![
            Atom {
                source_section: "Context".into(),
                ..sample_atom(AtomKind::FrictionPoint)
            },
            Atom {
                source_section: "Implementation Plan".into(),
                kind: AtomKind::Phase,
                ..sample_atom(AtomKind::Phase)
            },
            Atom {
                source_section: "Implementation Plan".into(),
                kind: AtomKind::Phase,
                title: "Phase 2".into(),
                ..sample_atom(AtomKind::Phase)
            },
        ];
        let specs = decompose(&atoms, "ADR-001");
        let groups = group_by_thread(&specs);

        assert!(groups.contains_key("context"));
        assert!(groups.contains_key("implementation"));
        assert_eq!(groups["implementation"].len(), 2);
    }

    #[test]
    fn bead_spec_serde_roundtrip() {
        let spec = BeadSpec {
            title: "[ADR-001] Test".into(),
            description: "body".into(),
            issue_type: "task".into(),
            priority: 1,
            channel: BdrChannel::Thread,
            thread_group: "implementation".into(),
            source_atom: AtomKind::Phase,
            derived_from: vec![adr_source("ADR-001")],
            source_line: 42,
            references: vec!["mache-85t".into()],
            target_repo: Some("mache".into()),
            depends_on: vec!["ADR-A".into()],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        let json = serde_json::to_string(&spec).unwrap();
        let back: BeadSpec = serde_json::from_str(&json).unwrap();
        assert_eq!(spec, back);
    }

    #[test]
    fn bead_spec_with_inferred_from_roundtrip() {
        use crate::provenance::InferenceTrace;
        let spec = BeadSpec {
            title: "[doc:spec.md] Build auth service".into(),
            description: "body".into(),
            issue_type: "feature".into(),
            priority: 1,
            channel: BdrChannel::Bead,
            thread_group: "general".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![ProvenanceRef::Doc {
                path: "docs/spec.md".into(),
            }],
            source_line: 5,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            success_criteria: vec![],
            inferred_from: Some(InferenceTrace {
                model: "claude-haiku-4-5".into(),
                rationale: Some("Section matched Phase pattern".into()),
            }),
            doc_coverage_min: None,
        };
        let json = serde_json::to_string(&spec).unwrap();
        let back: BeadSpec = serde_json::from_str(&json).unwrap();
        assert_eq!(spec, back);
    }

    #[test]
    fn decompose_with_meta_sets_target_repo() {
        let meta = DocMeta {
            repo: Some("leyline".into()),
            ..Default::default()
        };
        let atoms = vec![sample_atom(AtomKind::Phase)];
        let specs = decompose_with_meta(&atoms, "ADR-A", &meta);
        assert_eq!(specs[0].target_repo.as_deref(), Some("leyline"));
    }

    #[test]
    fn decompose_with_meta_sets_depends_on() {
        let meta = DocMeta {
            depends_on: vec!["ADR-A".into()],
            ..Default::default()
        };
        let atoms = vec![sample_atom(AtomKind::Decision)];
        let specs = decompose_with_meta(&atoms, "ADR-B", &meta);
        assert_eq!(specs[0].depends_on, vec!["ADR-A"]);
    }

    #[test]
    fn decompose_with_doc_provenance() {
        let meta = DocMeta {
            provenance: Some(ProvenanceRef::Doc {
                path: "docs/design/spec.md".into(),
            }),
            ..Default::default()
        };
        let atoms = vec![sample_atom(AtomKind::Phase)];
        let specs = decompose_with_meta(&atoms, "spec", &meta);
        assert_eq!(
            specs[0].derived_from,
            vec![ProvenanceRef::Doc {
                path: "docs/design/spec.md".into()
            }]
        );
    }

    #[test]
    fn atom_ref_overrides_meta_repo() {
        let meta = DocMeta {
            repo: Some("rosary".into()),
            ..Default::default()
        };
        let atom = Atom {
            references: vec!["mache:bead-85t".into()],
            ..sample_atom(AtomKind::Phase)
        };
        let specs = decompose_with_meta(&[atom], "ADR-A", &meta);
        assert_eq!(specs[0].target_repo.as_deref(), Some("mache"));
    }

    #[test]
    fn validation_atom_gets_success_criteria() {
        let atom = Atom {
            kind: AtomKind::ValidationPoint,
            title: "Cache hit rate".into(),
            body: "Cache hit rate > 90% for single-community commits\n`cargo test` passes".into(),
            source_line: 1,
            source_section: "Validation".into(),
            references: vec![],
        };
        let specs = decompose(&[atom], "ADR-A");
        assert!(!specs[0].success_criteria.is_empty());
        let criteria = &specs[0].success_criteria;
        // Should find the threshold
        assert!(criteria.iter().any(|c| c.threshold.is_some()));
        // Should find the command
        assert!(criteria.iter().any(|c| c.command.is_some()));
    }

    #[test]
    fn non_validation_atom_has_no_criteria() {
        let atom = sample_atom(AtomKind::Decision);
        let specs = decompose(&[atom], "ADR-A");
        assert!(specs[0].success_criteria.is_empty());
    }

    #[test]
    fn inferred_from_excluded_from_content_hash() {
        use crate::provenance::InferenceTrace;
        let base = BeadSpec {
            title: "Fix widget".into(),
            description: "broken".into(),
            issue_type: "bug".into(),
            priority: 1,
            channel: BdrChannel::Bead,
            thread_group: "core".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![adr_source("ADR-001")],
            source_line: 10,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        let with_inference = BeadSpec {
            inferred_from: Some(InferenceTrace {
                model: "claude-haiku-4-5".into(),
                rationale: Some("matched Phase".into()),
            }),
            ..base.clone()
        };
        assert_eq!(
            base.content_hash(),
            with_inference.content_hash(),
            "inferred_from must not affect content hash"
        );
    }

    #[test]
    fn derived_from_excluded_from_content_hash() {
        let spec1 = BeadSpec {
            title: "Fix widget".into(),
            description: "broken".into(),
            issue_type: "bug".into(),
            priority: 1,
            channel: BdrChannel::Bead,
            thread_group: "core".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![adr_source("ADR-001")],
            source_line: 10,
            references: vec!["ref-1".into()],
            target_repo: Some("mache".into()),
            depends_on: vec!["ADR-A".into()],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        let spec2 = BeadSpec {
            derived_from: vec![
                ProvenanceRef::Doc {
                    path: "docs/spec.md".into(),
                },
                adr_source("ADR-999"),
            ],
            source_line: 999,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            ..spec1.clone()
        };
        assert_eq!(
            spec1.content_hash(),
            spec2.content_hash(),
            "provenance changes must not affect content hash"
        );
    }

    #[test]
    fn content_hash_hex_is_64_chars() {
        let spec = BeadSpec {
            title: "t".into(),
            description: "d".into(),
            issue_type: "task".into(),
            priority: 2,
            channel: BdrChannel::Bead,
            thread_group: "g".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![adr_source("A")],
            source_line: 1,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        let hex = spec.content_hash_hex();
        assert_eq!(hex.len(), 64, "SHA-256 hex should be 64 chars: {hex}");
    }

    #[test]
    fn primary_source_returns_first() {
        let spec = BeadSpec {
            title: "t".into(),
            description: "d".into(),
            issue_type: "task".into(),
            priority: 1,
            channel: BdrChannel::Bead,
            thread_group: "g".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![
                adr_source("ADR-001"),
                ProvenanceRef::Doc {
                    path: "spec.md".into(),
                },
            ],
            source_line: 1,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        assert_eq!(spec.primary_source(), Some(&adr_source("ADR-001")));
    }

    #[test]
    fn primary_source_none_when_empty() {
        let spec = BeadSpec {
            title: "t".into(),
            description: "d".into(),
            issue_type: "task".into(),
            priority: 1,
            channel: BdrChannel::Bead,
            thread_group: "g".into(),
            source_atom: AtomKind::TechnicalSpec,
            derived_from: vec![],
            source_line: 1,
            references: vec![],
            target_repo: None,
            depends_on: vec![],
            success_criteria: vec![],
            inferred_from: None,
            doc_coverage_min: None,
        };
        assert!(spec.primary_source().is_none());
    }
}
