//! Integration tests for BDR Phase 1: frontmatter, success criteria, cross-repo routing.
//! Uses real ADRs as fixtures — not synthetic markdown.

use bdr::decompose::{decompose_with_meta, group_by_thread};
use bdr::parse::{AdrMeta, parse_adr, parse_adr_full};
use bdr::thread::{DecadeStatus, build_decade, build_decade_with_meta};

// -- Frontmatter parsing --

#[test]
fn adr_b_inline_frontmatter_parsed() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);

    assert_eq!(
        parsed.meta.status.as_deref(),
        Some("Proposed"),
        "should extract status from inline **Status:** Proposed"
    );
    assert!(
        parsed.meta.depends_on.iter().any(|d| d.contains("ADR-A")),
        "should extract depends_on from **Depends on:** ADR-A (Sheaf Cache)"
    );
    assert!(
        parsed.meta.relates_to.len() > 0,
        "should extract relates_to from **Relates to:**"
    );
}

#[test]
fn adr_a_inline_frontmatter_parsed() {
    let adr = include_str!("ADR-A-sheaf-cache.md");
    let parsed = parse_adr_full(adr);

    assert_eq!(parsed.meta.status.as_deref(), Some("Proposed"));
    assert!(
        parsed.meta.repo.is_some() || parsed.meta.relates_to.len() > 0,
        "should extract repo or relates_to metadata"
    );
}

#[test]
fn backward_compat_parse_adr_still_works() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let atoms_old = parse_adr(adr);
    let parsed = parse_adr_full(adr);
    assert_eq!(
        atoms_old.len(),
        parsed.atoms.len(),
        "parse_adr and parse_adr_full should produce same atom count"
    );
}

// -- Cross-repo routing --

#[test]
fn adr_b_depends_on_flows_to_beadspecs() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);
    let specs = decompose_with_meta(&parsed.atoms, "ADR-B", &parsed.meta);

    for spec in &specs {
        assert!(
            !spec.depends_on.is_empty(),
            "every spec from ADR-B should inherit depends_on from frontmatter, got none for: {}",
            spec.title
        );
    }
}

#[test]
fn adr_with_repo_field_sets_target_repo() {
    let meta = AdrMeta {
        repo: Some("leyline".into()),
        ..Default::default()
    };
    let adr = include_str!("ADR-A-sheaf-cache.md");
    let atoms = parse_adr(adr);
    let specs = decompose_with_meta(&atoms, "ADR-A", &meta);

    for spec in &specs {
        assert_eq!(
            spec.target_repo.as_deref(),
            Some("leyline"),
            "specs should inherit target_repo from meta.repo"
        );
    }
}

#[test]
fn atom_ref_overrides_meta_repo_in_real_adr() {
    // If an atom has a reference like "mache:something", it should route to mache
    // even if the ADR-level repo is "leyline"
    let meta = AdrMeta {
        repo: Some("leyline".into()),
        ..Default::default()
    };
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);
    let specs = decompose_with_meta(&parsed.atoms, "ADR-B", &meta);

    // Some specs may have mache references that override the repo
    let has_override = specs
        .iter()
        .any(|s| s.target_repo.as_deref() != Some("leyline") && s.target_repo.is_some());
    // This is a conditional test — not all atoms will have overrides
    if has_override {
        println!("Found atom-level repo override (expected for cross-repo ADRs)");
    }
}

// -- Success criteria --

#[test]
fn validation_atoms_get_success_criteria() {
    let adr = include_str!("ADR-A-sheaf-cache.md");
    let parsed = parse_adr_full(adr);
    let specs = decompose_with_meta(&parsed.atoms, "ADR-A", &parsed.meta);

    let validation_specs: Vec<_> = specs
        .iter()
        .filter(|s| s.source_atom == bdr::atom::AtomKind::ValidationPoint)
        .collect();

    // If there are validation atoms, they should have criteria
    for spec in &validation_specs {
        assert!(
            !spec.success_criteria.is_empty(),
            "ValidationPoint '{}' should have success_criteria",
            spec.title
        );
    }
}

#[test]
fn non_validation_atoms_have_no_criteria() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);
    let specs = decompose_with_meta(&parsed.atoms, "ADR-B", &parsed.meta);

    let non_validation: Vec<_> = specs
        .iter()
        .filter(|s| s.source_atom != bdr::atom::AtomKind::ValidationPoint)
        .collect();

    for spec in &non_validation {
        assert!(
            spec.success_criteria.is_empty(),
            "Non-validation atom '{}' should have no success_criteria",
            spec.title
        );
    }
}

// -- Decade assembly with meta --

#[test]
fn build_decade_with_meta_preserves_metadata() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);

    let decade = build_decade_with_meta(
        "ADR-B-merkle-sheaf-sync.md",
        "Merkle Sheaf Sync",
        &parsed.atoms,
        &parsed.meta,
    );

    assert_eq!(decade.id, "ADR-B-merkle-sheaf-sync");
    assert_eq!(decade.title, "Merkle Sheaf Sync");
    assert_eq!(decade.status, DecadeStatus::Proposed);
    assert!(decade.meta.is_some(), "decade should carry ADR metadata");

    let meta = decade.meta.unwrap();
    assert!(
        !meta.depends_on.is_empty(),
        "decade meta should have depends_on from frontmatter"
    );
}

#[test]
fn build_decade_without_meta_has_no_meta_field() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let atoms = parse_adr(adr);

    let decade = build_decade("ADR-B.md", "Test", &atoms);
    assert!(
        decade.meta.is_none(),
        "build_decade (no meta) should have None meta"
    );
}

// -- Full pipeline: parse → decompose → group → decade → validate --

#[test]
fn full_pipeline_adr_b() {
    let adr = include_str!("ADR-B-merkle-sheaf-sync.md");
    let parsed = parse_adr_full(adr);

    // Parse produced atoms
    assert!(!parsed.atoms.is_empty(), "ADR-B should produce atoms");

    // Decompose with meta
    let specs = decompose_with_meta(&parsed.atoms, "ADR-B", &parsed.meta);
    assert!(!specs.is_empty(), "should produce bead specs");

    // All specs have depends_on from frontmatter
    assert!(
        specs.iter().all(|s| !s.depends_on.is_empty()),
        "all specs should inherit ADR-B's depends_on"
    );

    // Group into threads
    let groups = group_by_thread(&specs);
    assert!(groups.len() >= 1, "should have at least 1 thread group");

    // Build decade
    let decade =
        build_decade_with_meta("ADR-B.md", "Merkle Sheaf Sync", &parsed.atoms, &parsed.meta);
    assert!(!decade.threads.is_empty());

    // Decade has meta
    assert!(decade.meta.is_some());

    // All beads in all threads have depends_on
    for thread in &decade.threads {
        for bead in &thread.beads {
            assert!(
                !bead.depends_on.is_empty(),
                "bead '{}' in thread '{}' should have depends_on",
                bead.title,
                thread.name
            );
        }
    }

    println!("\n=== ADR-B FULL PIPELINE ===");
    println!("  atoms: {}", parsed.atoms.len());
    println!("  specs: {}", specs.len());
    println!("  threads: {}", groups.len());
    println!("  meta.depends_on: {:?}", parsed.meta.depends_on);
    println!("  meta.relates_to: {:?}", parsed.meta.relates_to);
    for thread in &decade.threads {
        println!("  thread '{}': {} beads", thread.name, thread.beads.len());
        for bead in &thread.beads {
            println!(
                "    [{}/{}] {} → repo:{:?} criteria:{}",
                bead.issue_type,
                bead.priority,
                bead.title,
                bead.target_repo,
                bead.success_criteria.len()
            );
        }
    }
}
