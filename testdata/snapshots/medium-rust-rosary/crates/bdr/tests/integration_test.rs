use bdr::accrete::{CompletionEvent, CompletionOutcome, accrete};
use bdr::decompose::{decompose, group_by_thread};
use bdr::parse::parse_adr;
use bdr::thread::build_decade;

#[test]
fn end_to_end_adr001() {
    let adr = include_str!("../ADR-001-harmony-lattice-decomposition.md");

    // Step 1: Parse
    let atoms = parse_adr(adr);
    println!("\n=== ATOMS ({}) ===", atoms.len());
    for atom in &atoms {
        println!(
            "  [{:?}] {} (line {})",
            atom.kind, atom.title, atom.source_line
        );
    }
    assert!(!atoms.is_empty(), "should extract atoms from real ADR");

    // Step 2: Decompose
    let specs = decompose(&atoms, "ADR-001");
    println!("\n=== BEAD SPECS ({}) ===", specs.len());
    for spec in &specs {
        println!("  [{}] {} ({})", spec.channel, spec.title, spec.issue_type);
    }
    assert_eq!(specs.len(), atoms.len());

    // Step 3: Group into threads
    let groups = group_by_thread(&specs);
    println!("\n=== THREADS ({}) ===", groups.len());
    for (key, beads) in &groups {
        println!("  {}: {} beads", key, beads.len());
    }
    assert!(groups.len() > 1, "should have multiple thread groups");

    // Step 4: Build decade
    let decade = build_decade(
        "ADR-001-harmony-lattice-decomposition.md",
        "Harmony Lattice Decomposition",
        &atoms,
    );
    println!("\n=== DECADE ===");
    println!("  id: {}", decade.id);
    println!("  title: {}", decade.title);
    println!("  status: {:?}", decade.status);
    println!("  threads: {}", decade.threads.len());
    for thread in &decade.threads {
        println!(
            "    {} ({} beads, {} cross-repo refs)",
            thread.id,
            thread.beads.len(),
            thread.cross_repo_refs.len()
        );
    }

    // Step 5: Accrete — complete one bead, check transition
    let mut decade_mut = decade.clone();
    let first_bead = &decade.threads[0].beads[0];
    let event = CompletionEvent {
        bead_title: first_bead.title.clone(),
        thread_id: decade.threads[0].id.clone(),
        decade_id: decade.id.clone(),
        outcome: CompletionOutcome::Done,
    };
    accrete(&mut decade_mut, &[event]);
    println!("\n=== AFTER ACCRETION ===");
    println!(
        "  status: {:?} (was {:?})",
        decade_mut.status, decade.status
    );
}
