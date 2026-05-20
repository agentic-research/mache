// ADR atom types — the decomposable units of a decision record

use serde::{Deserialize, Serialize};

/// All possible atom kinds, used for exhaustive iteration in tests.
pub const ALL_ATOM_KINDS: [AtomKind; 9] = [
    AtomKind::FrictionPoint,
    AtomKind::Decision,
    AtomKind::Constraint,
    AtomKind::Consequence,
    AtomKind::Alternative,
    AtomKind::OpenQuestion,
    AtomKind::Phase,
    AtomKind::ValidationPoint,
    AtomKind::TechnicalSpec,
];

/// The kind of information atom extracted from an ADR.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum AtomKind {
    /// Problem that motivated the ADR.
    FrictionPoint,
    /// Chosen direction with explicit tradeoffs.
    Decision,
    /// Hard requirement that shaped the decision.
    Constraint,
    /// Positive or negative outcome of the decision.
    Consequence,
    /// Path considered and rejected, with reasoning.
    Alternative,
    /// Explicit unknown to address later.
    OpenQuestion,
    /// Implementation phase or milestone.
    Phase,
    /// Success criteria or acceptance test.
    ValidationPoint,
    /// Concrete technical detail: wire format, algorithm, schema.
    TechnicalSpec,
}

impl AtomKind {
    /// Lowercase string representation.
    pub fn as_str(&self) -> &str {
        match self {
            Self::FrictionPoint => "friction_point",
            Self::Decision => "decision",
            Self::Constraint => "constraint",
            Self::Consequence => "consequence",
            Self::Alternative => "alternative",
            Self::OpenQuestion => "open_question",
            Self::Phase => "phase",
            Self::ValidationPoint => "validation_point",
            Self::TechnicalSpec => "technical_spec",
        }
    }

    /// Suggested bead issue_type for this atom kind.
    pub fn suggested_issue_type(&self) -> &str {
        match self {
            Self::FrictionPoint => "task",
            Self::Decision => "task",
            Self::Constraint => "task",
            Self::Consequence => "task",
            Self::Alternative => "task",
            Self::OpenQuestion => "feature",
            Self::Phase => "epic",
            Self::ValidationPoint => "review",
            Self::TechnicalSpec => "task",
        }
    }

    /// Suggested bead priority (0=P0 highest, 3=P3 lowest).
    pub fn suggested_priority(&self) -> u8 {
        match self {
            Self::FrictionPoint => 1,
            Self::Decision => 1,
            Self::Constraint => 1,
            Self::Consequence => 2,
            Self::Alternative => 3,
            Self::OpenQuestion => 2,
            Self::Phase => 1,
            Self::ValidationPoint => 2,
            Self::TechnicalSpec => 2,
        }
    }
}

/// An atomic unit of information extracted from an ADR.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Atom {
    pub kind: AtomKind,
    pub title: String,
    pub body: String,
    pub source_line: usize,
    pub source_section: String,
    pub references: Vec<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    const VALID_ISSUE_TYPES: &[&str] = &["bug", "task", "feature", "epic", "review"];

    #[test]
    fn all_variants_covered() {
        assert_eq!(ALL_ATOM_KINDS.len(), 9);
    }

    #[test]
    fn as_str_non_empty() {
        for kind in ALL_ATOM_KINDS {
            assert!(!kind.as_str().is_empty(), "{kind:?} has empty as_str");
        }
    }

    #[test]
    fn suggested_issue_type_valid() {
        for kind in ALL_ATOM_KINDS {
            let it = kind.suggested_issue_type();
            assert!(
                VALID_ISSUE_TYPES.contains(&it),
                "{kind:?} mapped to invalid issue_type {it:?}"
            );
        }
    }

    #[test]
    fn suggested_priority_in_range() {
        for kind in ALL_ATOM_KINDS {
            let p = kind.suggested_priority();
            assert!(p <= 3, "{kind:?} mapped to out-of-range priority {p}");
        }
    }

    #[test]
    fn friction_point_mapping() {
        assert_eq!(AtomKind::FrictionPoint.suggested_issue_type(), "task");
        assert_eq!(AtomKind::FrictionPoint.suggested_priority(), 1);
    }

    #[test]
    fn phase_mapping() {
        assert_eq!(AtomKind::Phase.suggested_issue_type(), "epic");
        assert_eq!(AtomKind::Phase.suggested_priority(), 1);
    }

    #[test]
    fn validation_point_mapping() {
        assert_eq!(AtomKind::ValidationPoint.suggested_issue_type(), "review");
        assert_eq!(AtomKind::ValidationPoint.suggested_priority(), 2);
    }

    #[test]
    fn atom_serde_roundtrip() {
        let atom = Atom {
            kind: AtomKind::Decision,
            title: "Use Harmony format".into(),
            body: "Because it gives us constraints for free".into(),
            source_line: 42,
            source_section: "Decision".into(),
            references: vec!["mache-85t".into(), "openai-harmony".into()],
        };
        let json = serde_json::to_string(&atom).unwrap();
        let back: Atom = serde_json::from_str(&json).unwrap();
        assert_eq!(atom, back);
    }
}
