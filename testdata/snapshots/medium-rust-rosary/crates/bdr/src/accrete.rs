// Bead completion → decade state updates (bottom-up flow)

use std::collections::HashSet;

use crate::thread::{Decade, DecadeStatus, Thread};
use serde::{Deserialize, Serialize};

/// An event representing a bead completion.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CompletionEvent {
    pub bead_title: String,
    pub thread_id: String,
    pub decade_id: String,
    pub outcome: CompletionOutcome,
}

/// How a bead was completed.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum CompletionOutcome {
    Done,
    Rejected,
    Blocked,
    Stale,
}

/// Compute thread completion percentage.
/// Only `Done` outcomes count as completed.
pub fn thread_progress(thread: &Thread, completed: &HashSet<String>) -> f64 {
    if thread.beads.is_empty() {
        return 1.0; // vacuously complete
    }
    let done = thread
        .beads
        .iter()
        .filter(|b| completed.contains(&b.title))
        .count();
    done as f64 / thread.beads.len() as f64
}

/// Compute decade completion percentage (average of thread progress).
pub fn decade_progress(decade: &Decade, completed: &HashSet<String>) -> f64 {
    if decade.threads.is_empty() {
        return 1.0; // vacuously complete
    }
    let total: f64 = decade
        .threads
        .iter()
        .map(|t| thread_progress(t, completed))
        .sum();
    total / decade.threads.len() as f64
}

/// Determine if decade status should transition based on completion.
pub fn should_transition(decade: &Decade, completed: &HashSet<String>) -> Option<DecadeStatus> {
    let progress = decade_progress(decade, completed);

    match decade.status {
        DecadeStatus::Proposed => {
            if progress > 0.0 && progress < 1.0 {
                Some(DecadeStatus::Active)
            } else if progress >= 1.0 {
                Some(DecadeStatus::Completed)
            } else {
                None
            }
        }
        DecadeStatus::Active => {
            if progress >= 1.0 {
                Some(DecadeStatus::Completed)
            } else {
                None
            }
        }
        DecadeStatus::Completed | DecadeStatus::Superseded => None,
    }
}

/// Apply completion events to a decade, updating its status if warranted.
pub fn accrete(decade: &mut Decade, events: &[CompletionEvent]) {
    let completed: HashSet<String> = events
        .iter()
        .filter(|e| e.outcome == CompletionOutcome::Done && e.decade_id == decade.id)
        .map(|e| e.bead_title.clone())
        .collect();

    if let Some(new_status) = should_transition(decade, &completed) {
        decade.status = new_status;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::atom::{Atom, AtomKind};
    use crate::thread::build_decade;

    fn test_decade() -> Decade {
        let atoms = vec![
            Atom {
                kind: AtomKind::FrictionPoint,
                title: "Problem".into(),
                body: "Details".into(),
                source_line: 1,
                source_section: "Context".into(),
                references: vec![],
            },
            Atom {
                kind: AtomKind::Phase,
                title: "Phase 1".into(),
                body: "Do stuff".into(),
                source_line: 10,
                source_section: "Implementation Plan".into(),
                references: vec![],
            },
            Atom {
                kind: AtomKind::Phase,
                title: "Phase 2".into(),
                body: "More stuff".into(),
                source_line: 15,
                source_section: "Implementation Plan".into(),
                references: vec![],
            },
        ];
        build_decade("ADR-001.md", "Test", &atoms)
    }

    fn bead_titles(decade: &Decade) -> Vec<String> {
        decade
            .threads
            .iter()
            .flat_map(|t| t.beads.iter().map(|b| b.title.clone()))
            .collect()
    }

    #[test]
    fn thread_progress_all_done() {
        let decade = test_decade();
        let titles: HashSet<String> = bead_titles(&decade).into_iter().collect();
        for thread in &decade.threads {
            let progress = thread_progress(thread, &titles);
            assert!((progress - 1.0).abs() < f64::EPSILON);
        }
    }

    #[test]
    fn thread_progress_half_done() {
        let decade = test_decade();
        let impl_thread = decade
            .threads
            .iter()
            .find(|t| t.id.contains("implementation"))
            .unwrap();
        assert_eq!(impl_thread.beads.len(), 2);

        let mut completed = HashSet::new();
        completed.insert(impl_thread.beads[0].title.clone());
        let progress = thread_progress(impl_thread, &completed);
        assert!((progress - 0.5).abs() < f64::EPSILON);
    }

    #[test]
    fn thread_progress_empty_is_complete() {
        let thread = Thread {
            id: "test/empty".into(),
            name: "Empty".into(),
            decade_id: "test".into(),
            beads: vec![],
            cross_repo_refs: vec![],
        };
        let progress = thread_progress(&thread, &HashSet::new());
        assert!((progress - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn decade_progress_all_done() {
        let decade = test_decade();
        let titles: HashSet<String> = bead_titles(&decade).into_iter().collect();
        let progress = decade_progress(&decade, &titles);
        assert!((progress - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn decade_progress_empty_is_complete() {
        let decade = build_decade("ADR-001.md", "Empty", &[]);
        let progress = decade_progress(&decade, &HashSet::new());
        assert!((progress - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn should_transition_proposed_to_active() {
        let decade = test_decade();
        let mut completed = HashSet::new();
        // Complete one bead but not all
        completed.insert(bead_titles(&decade)[0].clone());
        let transition = should_transition(&decade, &completed);
        assert_eq!(transition, Some(DecadeStatus::Active));
    }

    #[test]
    fn should_transition_to_completed() {
        let mut decade = test_decade();
        decade.status = DecadeStatus::Active;
        let completed: HashSet<String> = bead_titles(&decade).into_iter().collect();
        let transition = should_transition(&decade, &completed);
        assert_eq!(transition, Some(DecadeStatus::Completed));
    }

    #[test]
    fn no_transition_when_already_completed() {
        let mut decade = test_decade();
        decade.status = DecadeStatus::Completed;
        let completed: HashSet<String> = bead_titles(&decade).into_iter().collect();
        assert_eq!(should_transition(&decade, &completed), None);
    }

    #[test]
    fn accrete_updates_status() {
        let mut decade = test_decade();
        assert_eq!(decade.status, DecadeStatus::Proposed);

        let titles = bead_titles(&decade);
        let events: Vec<CompletionEvent> = titles
            .into_iter()
            .map(|t| CompletionEvent {
                bead_title: t,
                thread_id: "any".into(),
                decade_id: decade.id.clone(),
                outcome: CompletionOutcome::Done,
            })
            .collect();

        accrete(&mut decade, &events);
        assert_eq!(decade.status, DecadeStatus::Completed);
    }

    #[test]
    fn rejected_beads_dont_count() {
        let mut decade = test_decade();
        let titles = bead_titles(&decade);
        let events: Vec<CompletionEvent> = titles
            .into_iter()
            .map(|t| CompletionEvent {
                bead_title: t,
                thread_id: "any".into(),
                decade_id: decade.id.clone(),
                outcome: CompletionOutcome::Rejected,
            })
            .collect();

        accrete(&mut decade, &events);
        assert_eq!(decade.status, DecadeStatus::Proposed); // no change
    }
}
