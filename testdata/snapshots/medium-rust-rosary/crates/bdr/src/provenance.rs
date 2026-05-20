// Polymorphic source reference for a BeadSpec.
//
// `derived_from: Vec<ProvenanceRef>` on BeadSpec encodes the ordered provenance
// chain — earlier entries were processed first. The first entry is the primary
// source; subsequent entries are secondary docs pulled in (cross-references,
// Depends-on ADRs resolved at parse time, future: imported SPECs).
//
// `InferenceTrace` captures the LLM classification step when deterministic
// parsing was insufficient. It is always posterior to `derived_from` by field
// position and is excluded from BeadSpec::content_hash.
//
// Serde: tagged with "kind" so JSON stays self-describing across variants.
// Future variants (GitHub Issue, Linear issue, RFD) follow the same pattern.

use serde::{Deserialize, Serialize};

/// Where a BeadSpec originated (one entry per processed document or source).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ProvenanceRef {
    /// Sourced from an Architecture Decision Record.
    Adr {
        /// The ADR identifier, e.g. "ADR-001" or "0007-bdr-enrichment-pipeline".
        id: String,
    },
    /// Sourced from a general structured document (design spec, SPEC.md, RFD, etc.)
    /// that is not ADR-shaped. The path is relative to the repo root when possible.
    Doc {
        /// File path, e.g. "docs/design/x-ray-spec.md".
        path: String,
    },
    /// Sourced from a Slack thread permalink.
    SlackThread {
        /// Permalink URL of the originating Slack thread.
        url: String,
        /// Optional human-readable description of the thread.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        summary: Option<String>,
    },
    /// Sourced from a meeting note.
    Meeting {
        /// Meeting title or document name.
        title: String,
        /// ISO 8601 date string, e.g. "2026-04-12".
        #[serde(default, skip_serializing_if = "Option::is_none")]
        date: Option<String>,
    },
    /// Manually created without a structured upstream source.
    Manual {
        /// Free-form note describing the origin of the work item.
        note: String,
    },
    /// Sourced from a conversation or agent session transcript.
    Session {
        /// Path to the transcript file, relative to repo root when possible.
        transcript_path: String,
        /// Optional human-readable summary of the session.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        summary: Option<String>,
    },
    /// Sourced from an existing code symbol or file — used for round-trip
    /// design recovery (code → atoms) and assay integration.
    Code {
        /// Repository name (short name, e.g. "rosary").
        repo: String,
        /// File path relative to repo root, e.g. "src/bead.rs".
        path: String,
        /// Optional symbol name within the file, e.g. "BeadSpec::content_hash".
        #[serde(default, skip_serializing_if = "Option::is_none")]
        symbol: Option<String>,
    },
}

/// Records that the LLM classifier was invoked because deterministic parsing
/// was insufficient to classify a section. Always posterior to `derived_from`
/// in the provenance chain. Excluded from BeadSpec::content_hash.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct InferenceTrace {
    /// Model identifier, e.g. "claude-haiku-4-5".
    pub model: String,
    /// LLM's rationale for its classification decision (for human/agent review).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rationale: Option<String>,
}

impl ProvenanceRef {
    /// Return a compact human-readable label for display and logging.
    pub fn label(&self) -> String {
        match self {
            ProvenanceRef::Adr { id } => format!("adr:{id}"),
            ProvenanceRef::Doc { path } => format!("doc:{path}"),
            ProvenanceRef::SlackThread { url, .. } => format!("slack:{url}"),
            ProvenanceRef::Meeting { title, date } => match date {
                Some(d) => format!("meeting:{title} ({d})"),
                None => format!("meeting:{title}"),
            },
            ProvenanceRef::Manual { note } => {
                if note.is_empty() {
                    "manual".to_string()
                } else {
                    format!("manual:{}", truncate(note, 40))
                }
            }
            ProvenanceRef::Session {
                transcript_path, ..
            } => format!("session:{transcript_path}"),
            ProvenanceRef::Code { repo, path, symbol } => match symbol {
                Some(sym) => format!("code:{repo}:{path}::{sym}"),
                None => format!("code:{repo}:{path}"),
            },
        }
    }

    /// If this is an ADR provenance, return the ADR id.
    pub fn adr_id(&self) -> Option<&str> {
        match self {
            ProvenanceRef::Adr { id } => Some(id.as_str()),
            _ => None,
        }
    }

    /// If this is a Doc provenance, return the file path.
    pub fn doc_path(&self) -> Option<&str> {
        match self {
            ProvenanceRef::Doc { path } => Some(path.as_str()),
            _ => None,
        }
    }
}

fn truncate(s: &str, max: usize) -> &str {
    if s.len() <= max {
        s
    } else {
        let end = s
            .char_indices()
            .take(max)
            .last()
            .map(|(i, c)| i + c.len_utf8())
            .unwrap_or(0);
        &s[..end]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn adr_label() {
        let p = ProvenanceRef::Adr {
            id: "ADR-001".into(),
        };
        assert_eq!(p.label(), "adr:ADR-001");
    }

    #[test]
    fn doc_label() {
        let p = ProvenanceRef::Doc {
            path: "docs/design/x-ray-spec.md".into(),
        };
        assert_eq!(p.label(), "doc:docs/design/x-ray-spec.md");
    }

    #[test]
    fn manual_empty_label() {
        let p = ProvenanceRef::Manual {
            note: String::new(),
        };
        assert_eq!(p.label(), "manual");
    }

    #[test]
    fn adr_id_accessor() {
        let p = ProvenanceRef::Adr { id: "0007".into() };
        assert_eq!(p.adr_id(), Some("0007"));
        let m = ProvenanceRef::Manual { note: "x".into() };
        assert!(m.adr_id().is_none());
    }

    #[test]
    fn doc_path_accessor() {
        let p = ProvenanceRef::Doc {
            path: "docs/spec.md".into(),
        };
        assert_eq!(p.doc_path(), Some("docs/spec.md"));
        let a = ProvenanceRef::Adr {
            id: "ADR-001".into(),
        };
        assert!(a.doc_path().is_none());
    }

    #[test]
    fn serde_roundtrip_adr() {
        let p = ProvenanceRef::Adr {
            id: "ADR-001".into(),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_doc() {
        let p = ProvenanceRef::Doc {
            path: "docs/design/spec.md".into(),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_slack() {
        let p = ProvenanceRef::SlackThread {
            url: "https://slack/archives/C123/p456".into(),
            summary: Some("discussed caching approach".into()),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_manual_no_note() {
        let p = ProvenanceRef::Manual {
            note: String::new(),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn tagged_json_shape() {
        let p = ProvenanceRef::Adr {
            id: "ADR-007".into(),
        };
        let v: serde_json::Value = serde_json::to_value(&p).unwrap();
        assert_eq!(v["kind"], "adr");
        assert_eq!(v["id"], "ADR-007");
    }

    #[test]
    fn tagged_json_doc_shape() {
        let p = ProvenanceRef::Doc {
            path: "docs/spec.md".into(),
        };
        let v: serde_json::Value = serde_json::to_value(&p).unwrap();
        assert_eq!(v["kind"], "doc");
        assert_eq!(v["path"], "docs/spec.md");
    }

    #[test]
    fn inference_trace_serde_roundtrip() {
        let t = InferenceTrace {
            model: "claude-haiku-4-5".into(),
            rationale: Some("Section 'Sub-Project A' matched Phase pattern".into()),
        };
        let json = serde_json::to_string(&t).unwrap();
        let back: InferenceTrace = serde_json::from_str(&json).unwrap();
        assert_eq!(t, back);
    }

    #[test]
    fn inference_trace_no_rationale() {
        let t = InferenceTrace {
            model: "claude-haiku-4-5".into(),
            rationale: None,
        };
        let json = serde_json::to_string(&t).unwrap();
        assert!(!json.contains("rationale"));
        let back: InferenceTrace = serde_json::from_str(&json).unwrap();
        assert_eq!(t, back);
    }

    #[test]
    fn serde_roundtrip_session() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/2026-04-29.md".into(),
            summary: Some("architecture handoff discussion".into()),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_session_no_summary() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/2026-04-29.md".into(),
            summary: None,
        };
        let json = serde_json::to_string(&p).unwrap();
        assert!(!json.contains("summary"));
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_code() {
        let p = ProvenanceRef::Code {
            repo: "rosary".into(),
            path: "src/bead.rs".into(),
            symbol: Some("BeadSpec::content_hash".into()),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn serde_roundtrip_code_no_symbol() {
        let p = ProvenanceRef::Code {
            repo: "mache".into(),
            path: "cmd/server/main.go".into(),
            symbol: None,
        };
        let json = serde_json::to_string(&p).unwrap();
        assert!(!json.contains("symbol"));
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn label_session() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/2026-04-29.md".into(),
            summary: None,
        };
        assert_eq!(p.label(), "session:sessions/2026-04-29.md");
    }

    #[test]
    fn label_code_with_symbol() {
        let p = ProvenanceRef::Code {
            repo: "rosary".into(),
            path: "src/bead.rs".into(),
            symbol: Some("BeadSpec".into()),
        };
        assert_eq!(p.label(), "code:rosary:src/bead.rs::BeadSpec");
    }

    #[test]
    fn label_code_no_symbol() {
        let p = ProvenanceRef::Code {
            repo: "rosary".into(),
            path: "src/bead.rs".into(),
            symbol: None,
        };
        assert_eq!(p.label(), "code:rosary:src/bead.rs");
    }

    #[test]
    fn tagged_json_session_shape() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/x.md".into(),
            summary: None,
        };
        let v: serde_json::Value = serde_json::to_value(&p).unwrap();
        assert_eq!(v["kind"], "session");
        assert_eq!(v["transcript_path"], "sessions/x.md");
    }

    #[test]
    fn tagged_json_code_shape() {
        let p = ProvenanceRef::Code {
            repo: "rosary".into(),
            path: "src/bead.rs".into(),
            symbol: Some("BeadSpec".into()),
        };
        let v: serde_json::Value = serde_json::to_value(&p).unwrap();
        assert_eq!(v["kind"], "code");
        assert_eq!(v["repo"], "rosary");
        assert_eq!(v["path"], "src/bead.rs");
        assert_eq!(v["symbol"], "BeadSpec");
    }
}
