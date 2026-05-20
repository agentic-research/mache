// Thread grouping + cross-repo routing

use crate::atom::Atom;
use crate::decompose::{BeadSpec, decompose_with_meta, group_by_thread};
use crate::parse::AdrMeta;
use serde::{Deserialize, Serialize};

/// A thread is a semantic grouping of related beads within a decade.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Thread {
    pub id: String,
    pub name: String,
    pub decade_id: String,
    pub beads: Vec<BeadSpec>,
    pub cross_repo_refs: Vec<String>,
}

/// A decade is one ADR decomposed — the top-level organizing primitive.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Decade {
    pub id: String,
    pub title: String,
    pub source_path: String,
    pub threads: Vec<Thread>,
    pub status: DecadeStatus,
    /// ADR metadata (frontmatter: depends_on, relates_to, target repo).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub meta: Option<AdrMeta>,
}

/// Overall status of a decade.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum DecadeStatus {
    Proposed,
    Active,
    Completed,
    Superseded,
}

/// Thread ordering for consistent display.
const THREAD_ORDER: &[&str] = &[
    "context",
    "decision",
    "alternatives",
    "implementation",
    "validation",
    "open-questions",
    "consequences",
    "general",
];

/// Build a decade from parsed atoms (backward-compatible, no meta).
pub fn build_decade(adr_path: &str, adr_title: &str, atoms: &[Atom]) -> Decade {
    build_decade_with_meta(adr_path, adr_title, atoms, &AdrMeta::default())
}

/// Build a decade from parsed atoms with ADR metadata.
pub fn build_decade_with_meta(
    adr_path: &str,
    adr_title: &str,
    atoms: &[Atom],
    meta: &AdrMeta,
) -> Decade {
    let adr_id = derive_adr_id(adr_path);
    let specs = decompose_with_meta(atoms, &adr_id, meta);
    let groups = group_by_thread(&specs);

    let mut threads: Vec<Thread> = groups
        .into_iter()
        .map(|(group_key, bead_refs)| {
            let beads: Vec<BeadSpec> = bead_refs.into_iter().cloned().collect();
            let cross_repo_refs = extract_cross_repo_refs(&beads);

            Thread {
                id: format!("{}/{}", adr_id, group_key),
                name: format!("{}: {}", adr_title, capitalize(&group_key)),
                decade_id: adr_id.clone(),
                beads,
                cross_repo_refs,
            }
        })
        .collect();

    // Sort threads by conventional order
    threads.sort_by_key(|t| {
        let group = t.id.rsplit('/').next().unwrap_or("");
        THREAD_ORDER
            .iter()
            .position(|&o| o == group)
            .unwrap_or(THREAD_ORDER.len())
    });

    let has_meta = meta != &AdrMeta::default();

    Decade {
        id: adr_id,
        title: adr_title.to_string(),
        source_path: adr_path.to_string(),
        threads,
        status: DecadeStatus::Proposed,
        meta: if has_meta { Some(meta.clone()) } else { None },
    }
}

/// Derive an ADR ID from a file path.
fn derive_adr_id(path: &str) -> String {
    let filename = path.rsplit('/').next().unwrap_or(path);
    filename.strip_suffix(".md").unwrap_or(filename).to_string()
}

/// Extract cross-repo references from bead specs.
fn extract_cross_repo_refs(specs: &[BeadSpec]) -> Vec<String> {
    let mut refs: Vec<String> = specs
        .iter()
        .flat_map(|s| s.references.iter())
        .filter(|r| r.contains(':') || r.contains('/'))
        .cloned()
        .collect();
    refs.sort();
    refs.dedup();
    refs
}

/// Capitalize first letter.
fn capitalize(s: &str) -> String {
    let mut c = s.chars();
    match c.next() {
        None => String::new(),
        Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::atom::AtomKind;

    fn sample_atoms() -> Vec<Atom> {
        vec![
            Atom {
                kind: AtomKind::FrictionPoint,
                title: "ADRs don't connect to work".into(),
                body: "Problem description".into(),
                source_line: 10,
                source_section: "Context".into(),
                references: vec![],
            },
            Atom {
                kind: AtomKind::Phase,
                title: "Phase 1: Scaffold".into(),
                body: "Create crate skeleton".into(),
                source_line: 50,
                source_section: "Implementation Plan".into(),
                references: vec!["mache:bead-85t".into()],
            },
            Atom {
                kind: AtomKind::Phase,
                title: "Phase 2: Decompose".into(),
                body: "Wire to Dolt".into(),
                source_line: 55,
                source_section: "Implementation Plan".into(),
                references: vec![],
            },
            Atom {
                kind: AtomKind::OpenQuestion,
                title: "Should decade be renamed?".into(),
                body: "Naming concern".into(),
                source_line: 70,
                source_section: "Open Questions".into(),
                references: vec![],
            },
            Atom {
                kind: AtomKind::ValidationPoint,
                title: "33 tests pass".into(),
                body: "All unit tests green".into(),
                source_line: 80,
                source_section: "Validation".into(),
                references: vec![],
            },
        ]
    }

    #[test]
    fn build_decade_produces_threads() {
        let decade = build_decade("docs/ADR-001.md", "Use Harmony", &sample_atoms());
        assert!(!decade.threads.is_empty());
        assert_eq!(decade.id, "ADR-001");
        assert_eq!(decade.title, "Use Harmony");
        assert_eq!(decade.status, DecadeStatus::Proposed);
    }

    #[test]
    fn threads_have_unique_ids() {
        let decade = build_decade("ADR-001.md", "Test", &sample_atoms());
        let ids: Vec<_> = decade.threads.iter().map(|t| &t.id).collect();
        let mut deduped = ids.clone();
        deduped.dedup();
        assert_eq!(ids.len(), deduped.len(), "thread IDs must be unique");
    }

    #[test]
    fn cross_repo_refs_extracted() {
        let decade = build_decade("ADR-001.md", "Test", &sample_atoms());
        let impl_thread = decade
            .threads
            .iter()
            .find(|t| t.id.contains("implementation"));
        assert!(impl_thread.is_some());
        assert!(
            impl_thread
                .unwrap()
                .cross_repo_refs
                .contains(&"mache:bead-85t".to_string())
        );
    }

    #[test]
    fn empty_atoms_produce_empty_decade() {
        let decade = build_decade("ADR-001.md", "Empty", &[]);
        assert!(decade.threads.is_empty());
        assert_eq!(decade.status, DecadeStatus::Proposed);
    }

    #[test]
    fn threads_ordered_conventionally() {
        let decade = build_decade("ADR-001.md", "Test", &sample_atoms());
        let group_keys: Vec<_> = decade
            .threads
            .iter()
            .map(|t| t.id.rsplit('/').next().unwrap_or("").to_string())
            .collect();

        // context should come before implementation
        if let (Some(ctx_pos), Some(impl_pos)) = (
            group_keys.iter().position(|k| k == "context"),
            group_keys.iter().position(|k| k == "implementation"),
        ) {
            assert!(
                ctx_pos < impl_pos,
                "context should come before implementation"
            );
        }
    }

    #[test]
    fn thread_names_are_readable() {
        let decade = build_decade("ADR-001.md", "Use Harmony", &sample_atoms());
        for thread in &decade.threads {
            assert!(
                thread.name.starts_with("Use Harmony:"),
                "thread name should start with ADR title"
            );
        }
    }

    #[test]
    fn decade_serde_roundtrip() {
        let decade = build_decade("ADR-001.md", "Test", &sample_atoms());
        let json = serde_json::to_string(&decade).unwrap();
        let back: Decade = serde_json::from_str(&json).unwrap();
        assert_eq!(decade, back);
    }
}
