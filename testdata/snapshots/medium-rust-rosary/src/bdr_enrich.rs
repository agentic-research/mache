//! LLM-assisted BDR atom extraction for non-ADR documents.
//!
//! When `rsry decompose --model <haiku|sonnet>` is used and the document is not
//! ADR-shaped (no ## Status / ## Context / ## Decision headings), this module
//! spawns `claude -p` to extract atoms from arbitrary structured docs (design
//! specs, SDDs, roadmaps, README sections).
//!
//! The heuristic path (`bdr::parse::parse_adr_full`) remains the default and
//! is always used for ADR-shaped documents regardless of `--model`.

use anyhow::{Context, Result};
use bdr::atom::{Atom, AtomKind};
use serde::Deserialize;

const SYSTEM_PROMPT: &str = r#"You are a BDR (Bead Decision Record) decomposition engine.
Extract actionable work items from the provided document and return them as a JSON array.

Each element has these fields:
  kind          - one of the AtomKind values below (string)
  title         - short imperative title, max 60 chars
  body          - full description: what, why, acceptance criteria
  source_section - the document heading this item came from
  references    - array of strings: bead IDs, repo names, or related items mentioned in the text

AtomKind values and when to use each:
  Phase           - an implementation milestone or stage (produces an epic bead)
  TechnicalSpec   - concrete implementation detail: algorithm, schema, API contract, wire format
  ValidationPoint - success criterion, acceptance test, observable metric
  OpenQuestion    - explicit unknown that needs resolution before or during implementation
  FrictionPoint   - problem or pain point motivating the work (produces a task bead)
  Decision        - an explicit architectural choice being made
  Constraint      - hard requirement that limits design choices
  Consequence     - outcome or tradeoff of a decision
  Alternative     - an approach considered but rejected, with reasoning

Rules:
- Prefer Phase atoms for top-level milestones; nest detail as TechnicalSpec / ValidationPoint
- Every phase should have at least one ValidationPoint
- Skip pure prose/background sections with no actionable content
- Do NOT include the document title itself as an atom
- Return ONLY a JSON array, no prose, no markdown fences"#;

/// Spawn `claude -p` to extract BDR atoms from a non-ADR document.
///
/// Uses the installed `claude` CLI (same auth path as dispatch/verify).
/// The returned atoms feed directly into `bdr::decompose::decompose_with_meta`.
pub async fn extract_atoms_with_llm(markdown: &str, model: &str) -> Result<Vec<Atom>> {
    let model_id = resolve_model_id(model);
    eprintln!("[bdr-enrich] extracting atoms via {model_id}...");

    let prompt = format!("{SYSTEM_PROMPT}\n\nExtract BDR atoms from this document:\n\n{markdown}");

    let output = tokio::process::Command::new("claude")
        .args(["-p", &prompt, "--model", model_id, "--allowedTools", ""])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .output()
        .await
        .context("spawning claude subprocess for BDR extraction")?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("claude exited non-zero during BDR extraction: {stderr}");
    }

    let text = String::from_utf8(output.stdout).context("claude output not UTF-8")?;
    parse_llm_atoms(&text)
}

/// Map shorthand model names to full Anthropic model IDs.
pub fn resolve_model_id(model: &str) -> &str {
    match model {
        "haiku" => "claude-haiku-4-5-20251001",
        "sonnet" => "claude-sonnet-4-6",
        "opus" => "claude-opus-4-6",
        other => other, // pass through full IDs unchanged
    }
}

/// Parse the LLM JSON response into atoms.
///
/// Lenient: tolerates code fences and trailing prose. The model frequently
/// wraps output in ```json blocks, and when it has nothing to extract it
/// likes to emit `[]` followed by an explanatory paragraph.
fn parse_llm_atoms(text: &str) -> Result<Vec<Atom>> {
    let json_str = extract_json_array(text)
        .with_context(|| format!("could not find JSON array in LLM output:\n{text}"))?;

    #[derive(Deserialize)]
    struct LlmAtom {
        kind: String,
        title: String,
        body: String,
        source_section: String,
        #[serde(default)]
        references: Vec<String>,
    }

    let llm_atoms: Vec<LlmAtom> = serde_json::from_str(&json_str)
        .with_context(|| format!("parsing LLM atom JSON:\n{json_str}"))?;

    eprintln!("[bdr-enrich] extracted {} atoms", llm_atoms.len());

    Ok(llm_atoms
        .into_iter()
        .enumerate()
        .map(|(i, a)| Atom {
            kind: parse_atom_kind(&a.kind),
            title: a.title,
            body: a.body,
            source_line: i + 1,
            source_section: a.source_section,
            references: a.references,
        })
        .collect())
}

/// Extract the first balanced top-level JSON array from `text`, tolerating
/// markdown fences before/after and any trailing prose the model adds.
///
/// Tracks string and escape state so brackets inside strings don't confuse
/// the depth counter. Returns the substring including the outer `[ ... ]`.
fn extract_json_array(text: &str) -> Option<String> {
    let bytes = text.as_bytes();
    let start = bytes.iter().position(|&b| b == b'[')?;
    let mut depth = 0i32;
    let mut in_string = false;
    let mut escape = false;
    for (i, &b) in bytes.iter().enumerate().skip(start) {
        if in_string {
            if escape {
                escape = false;
            } else if b == b'\\' {
                escape = true;
            } else if b == b'"' {
                in_string = false;
            }
            continue;
        }
        match b {
            b'"' => in_string = true,
            b'[' => depth += 1,
            b']' => {
                depth -= 1;
                if depth == 0 {
                    return Some(text[start..=i].to_string());
                }
            }
            _ => {}
        }
    }
    None
}

fn parse_atom_kind(s: &str) -> AtomKind {
    match s {
        "FrictionPoint" | "friction_point" => AtomKind::FrictionPoint,
        "Decision" | "decision" => AtomKind::Decision,
        "Constraint" | "constraint" => AtomKind::Constraint,
        "Consequence" | "consequence" => AtomKind::Consequence,
        "Alternative" | "alternative" => AtomKind::Alternative,
        "OpenQuestion" | "open_question" => AtomKind::OpenQuestion,
        "Phase" | "phase" => AtomKind::Phase,
        "ValidationPoint" | "validation_point" => AtomKind::ValidationPoint,
        "TechnicalSpec" | "technical_spec" => AtomKind::TechnicalSpec,
        _ => AtomKind::TechnicalSpec, // safe default for unrecognized kinds
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_atom_kind_handles_all_variants() {
        let cases = [
            ("FrictionPoint", AtomKind::FrictionPoint),
            ("friction_point", AtomKind::FrictionPoint),
            ("Phase", AtomKind::Phase),
            ("phase", AtomKind::Phase),
            ("ValidationPoint", AtomKind::ValidationPoint),
            ("OpenQuestion", AtomKind::OpenQuestion),
            ("TechnicalSpec", AtomKind::TechnicalSpec),
            ("unknown-kind", AtomKind::TechnicalSpec),
        ];
        for (input, expected) in cases {
            assert_eq!(parse_atom_kind(input), expected, "failed for {input}");
        }
    }

    #[test]
    fn resolve_model_id_maps_shorthand() {
        assert_eq!(resolve_model_id("haiku"), "claude-haiku-4-5-20251001");
        assert_eq!(resolve_model_id("sonnet"), "claude-sonnet-4-6");
        assert_eq!(resolve_model_id("claude-opus-4-6"), "claude-opus-4-6");
    }

    #[test]
    fn parse_llm_atoms_valid_json() {
        // Use escaped JSON (no raw string) to avoid r#"..."# delimiter collision with ## headings
        let json = "[\
            {\"kind\":\"Phase\",\"title\":\"Implement ingestion pipeline\",\
             \"body\":\"Build the data ingestion layer.\",\
             \"source_section\":\"Sub-Project A\",\"references\":[\"rosary-abc\"]},\
            {\"kind\":\"ValidationPoint\",\"title\":\"Ingestion processes 1k docs per sec\",\
             \"body\":\"Throughput at p99.\",\
             \"source_section\":\"Sub-Project A\",\"references\":[]}\
        ]";
        let atoms = parse_llm_atoms(json).unwrap();
        assert_eq!(atoms.len(), 2);
        assert_eq!(atoms[0].kind, AtomKind::Phase);
        assert_eq!(atoms[1].kind, AtomKind::ValidationPoint);
        assert_eq!(atoms[0].source_line, 1);
        assert_eq!(atoms[1].source_line, 2);
    }

    #[test]
    fn parse_llm_atoms_strips_code_fences() {
        let json = "```json\n[{\"kind\":\"Phase\",\"title\":\"T\",\"body\":\"B\",\"source_section\":\"S\",\"references\":[]}]\n```";
        let atoms = parse_llm_atoms(json).unwrap();
        assert_eq!(atoms.len(), 1);
    }

    #[test]
    fn parse_llm_atoms_invalid_json_returns_error() {
        let result = parse_llm_atoms("not json");
        assert!(result.is_err());
    }

    // ── lenient parser tests (rosary-bdr-enrich-prose) ────────────────────────

    #[test]
    fn parse_lenient_pure_empty_array() {
        let atoms = parse_llm_atoms("[]").unwrap();
        assert!(atoms.is_empty());
    }

    #[test]
    fn parse_lenient_array_with_trailing_prose() {
        // Real failure mode from `rsry capture --from-code` on a Widget struct:
        // LLM emits empty array then explains why.
        let raw = "[]\n\nThis code documents an existing implementation, not a design proposal.";
        let atoms = parse_llm_atoms(raw).unwrap();
        assert!(atoms.is_empty());
    }

    #[test]
    fn parse_lenient_fenced_with_trailing_prose() {
        let raw = "```json\n[]\n```\n\nNo actionable atoms found in this snippet.";
        let atoms = parse_llm_atoms(raw).unwrap();
        assert!(atoms.is_empty());
    }

    #[test]
    fn parse_lenient_array_with_string_containing_brackets() {
        // The bracket-counter must respect strings.
        let raw = r#"[{"kind":"Phase","title":"Use [brackets] in title","body":"B","source_section":"S","references":[]}] explanation here"#;
        let atoms = parse_llm_atoms(raw).unwrap();
        assert_eq!(atoms.len(), 1);
        assert!(atoms[0].title.contains("[brackets]"));
    }

    #[test]
    fn parse_lenient_no_array_at_all_errors() {
        let result = parse_llm_atoms("Sorry, I cannot find any atoms here.");
        assert!(result.is_err());
    }

    #[test]
    fn extract_json_array_handles_escaped_quotes_in_strings() {
        let raw = r#"[{"k":"has \"quotes\" and ]bracket"}] trailing"#;
        let extracted = extract_json_array(raw).unwrap();
        assert_eq!(extracted, r#"[{"k":"has \"quotes\" and ]bracket"}]"#);
    }
}
