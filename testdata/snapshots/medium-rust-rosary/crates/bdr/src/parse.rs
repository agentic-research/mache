// Document parser — extract atoms from structured markdown.
//
// Two parse paths, selected by `is_adr_shaped`:
//
// 1. ADR path (`parse_adr_full`): heading-keyword classifier maps sections to
//    AtomKind (Context→FrictionPoint, Decision→Decision, etc.). Produces high-
//    signal atoms with known provenance. Used for MADR/Nygard ADR conventions.
//
// 2. Generic path (`parse_generic_doc`): every `## Heading` body becomes an
//    atom, classified by keyword heuristics. Unrecognised headings produce
//    TechnicalSpec atoms. Used for design specs, SPEC.md, RFDs, meeting notes.
//    The LLM classifier in the enrichment pipeline (PR B) can upgrade these.
//
// `DocMeta` replaces the old `AdrMeta` and is source-type-agnostic.

use crate::atom::{Atom, AtomKind};
use crate::provenance::ProvenanceRef;
use serde::{Deserialize, Serialize};

/// Metadata extracted from a document's frontmatter or inline header fields.
/// Source-type agnostic: works for ADRs, design specs, SPEC.md, RFDs, etc.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DocMeta {
    /// Document status (Proposed, Accepted, Draft, etc.)
    pub status: Option<String>,
    /// Author name
    pub author: Option<String>,
    /// Date string
    pub date: Option<String>,
    /// Target repo for beads from this document
    pub repo: Option<String>,
    /// Document IDs this document depends on
    pub depends_on: Vec<String>,
    /// Document IDs this document relates to
    pub relates_to: Vec<String>,
    /// Explicit provenance override. When set, used as the primary `derived_from`
    /// entry instead of constructing one from the doc_id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provenance: Option<ProvenanceRef>,
}

// Backward-compatible alias — callers that imported AdrMeta by name continue to work.
pub type AdrMeta = DocMeta;

/// Result of parsing a document: metadata from frontmatter + atoms from body.
#[derive(Debug, Clone, PartialEq)]
pub struct ParsedDoc {
    pub meta: DocMeta,
    pub atoms: Vec<Atom>,
}

// Backward-compatible alias.
pub type ParsedAdr = ParsedDoc;

/// Detect whether a markdown document is shaped like an ADR.
///
/// ADRs follow the MADR/Nygard convention with headings like "## Status",
/// "## Context", "## Decision", "## Consequences". Design specs, SDDs, and
/// README-style docs use different heading conventions (## Sub-Project, ## Phase,
/// ## Hypothesis) and are better decomposed via the generic path.
///
/// Returns true if at least two ADR-standard headings are present.
pub fn is_adr_shaped(markdown: &str) -> bool {
    let adr_headings = [
        "## status",
        "## context",
        "## decision",
        "## consequences",
        "## alternatives",
        "## problem statement",
        "## proposed solution",
    ];
    let lower = markdown.to_lowercase();
    adr_headings.iter().filter(|h| lower.contains(*h)).count() >= 2
}

/// Parse any structured markdown document into atoms.
///
/// Routes to the ADR parser for ADR-shaped documents and to the generic
/// parser for everything else. The doc_path is used to construct the
/// `provenance` field in `DocMeta` when not already set.
pub fn parse_doc_full(markdown: &str, doc_path: &str) -> ParsedDoc {
    let mut parsed = if is_adr_shaped(markdown) {
        parse_adr_full(markdown)
    } else {
        parse_generic_doc(markdown)
    };

    // Set provenance from path if not already provided in frontmatter.
    if parsed.meta.provenance.is_none() {
        parsed.meta.provenance = Some(if is_adr_shaped(markdown) {
            // For ADR-shaped docs, try to extract an ADR ID from the path/title.
            let id = adr_id_from_path(doc_path);
            ProvenanceRef::Adr { id }
        } else {
            ProvenanceRef::Doc {
                path: doc_path.to_string(),
            }
        });
    }

    parsed
}

/// Parse an ADR markdown string into atoms (backward-compatible).
pub fn parse_adr(markdown: &str) -> Vec<Atom> {
    parse_adr_full(markdown).atoms
}

/// Parse an ADR markdown string into metadata + atoms.
pub fn parse_adr_full(markdown: &str) -> ParsedDoc {
    let (meta, body) = extract_frontmatter(markdown);
    let mut atoms = Vec::new();
    let lines: Vec<&str> = body.lines().collect();
    let sections = extract_sections(&lines);

    for section in &sections {
        let kind = classify_adr_section(section.heading);
        let section_atoms = extract_atoms_from_adr_section(section, kind);
        atoms.extend(section_atoms);
    }

    ParsedDoc { meta, atoms }
}

/// Parse a non-ADR structured document into atoms.
///
/// Every `## Heading` body becomes one atom, classified by keyword heuristics:
/// - Implementation/Phase/Plan/Step → Phase
/// - Goal/Objective/Problem/Friction → FrictionPoint
/// - Validation/Success/Metric/Acceptance → ValidationPoint
/// - Question/Unknown/Open → OpenQuestion
/// - Decision/Chosen/Selected → Decision
/// - Constraint/Requirement/Must → Constraint
/// - Everything else → TechnicalSpec
///
/// `### Subheadings` within a section are each extracted as their own atom
/// (they represent sub-tasks or sub-phases of the parent).
pub fn parse_generic_doc(markdown: &str) -> ParsedDoc {
    let (meta, body) = extract_frontmatter(markdown);
    let mut atoms = Vec::new();
    let lines: Vec<&str> = body.lines().collect();
    let sections = extract_sections(&lines);

    for section in &sections {
        let kind = classify_generic_section(section.heading);
        let section_atoms = extract_atoms_from_generic_section(section, kind);
        atoms.extend(section_atoms);
    }

    ParsedDoc { meta, atoms }
}

// ── ADR classification ────────────────────────────────────────────────────────

/// Known ADR section types, mapped from heading text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AdrSectionKind {
    Context,
    Decision,
    Consequences,
    Alternatives,
    OpenQuestions,
    Implementation,
    Validation,
    References,
    Status,
    Other,
}

/// Classify a section heading into a known ADR section kind.
fn classify_adr_section(heading: &str) -> AdrSectionKind {
    let lower = heading.to_lowercase();

    if lower.contains("context") || lower.contains("problem") || lower.contains("motivation") {
        AdrSectionKind::Context
    } else if lower.contains("decision") && !lower.contains("driver") {
        AdrSectionKind::Decision
    } else if lower.contains("consequence") || lower.contains("impact") {
        AdrSectionKind::Consequences
    } else if lower.contains("alternative") || lower.contains("option") {
        AdrSectionKind::Alternatives
    } else if lower.contains("open question") || lower.contains("unknown") {
        AdrSectionKind::OpenQuestions
    } else if lower.contains("implementation") || lower.contains("phase") || lower.contains("plan")
    {
        AdrSectionKind::Implementation
    } else if lower.contains("validation") || lower.contains("success") || lower.contains("metric")
    {
        AdrSectionKind::Validation
    } else if lower.contains("reference") || lower.contains("link") {
        AdrSectionKind::References
    } else if lower.contains("status") || lower.contains("date") || lower.contains("author") {
        AdrSectionKind::Status
    } else {
        AdrSectionKind::Other
    }
}

/// Extract atoms from an ADR-classified section.
fn extract_atoms_from_adr_section(section: &Section, kind: AdrSectionKind) -> Vec<Atom> {
    match kind {
        // These sections don't produce actionable atoms — they're observations,
        // rejected paths, or metadata. Creating beads from them produces noise.
        AdrSectionKind::References
        | AdrSectionKind::Status
        | AdrSectionKind::Other
        | AdrSectionKind::Consequences
        | AdrSectionKind::Alternatives => Vec::new(),

        AdrSectionKind::Context => extract_block_atoms(section, AtomKind::FrictionPoint),
        AdrSectionKind::Decision => extract_block_atoms(section, AtomKind::Decision),
        AdrSectionKind::OpenQuestions => extract_list_atoms(section, AtomKind::OpenQuestion),
        AdrSectionKind::Implementation => extract_subsection_atoms(section, AtomKind::Phase),
        AdrSectionKind::Validation => extract_list_atoms(section, AtomKind::ValidationPoint),
    }
}

// ── Generic doc classification ────────────────────────────────────────────────

/// Classify a section heading from a non-ADR document into an AtomKind.
fn classify_generic_section(heading: &str) -> AtomKind {
    let lower = heading.to_lowercase();

    if lower.contains("implement")
        || lower.contains("phase")
        || lower.contains("plan")
        || lower.contains("step")
        || lower.contains("milestone")
        || lower.contains("sub-project")
        || lower.contains("workstream")
    {
        AtomKind::Phase
    } else if lower.contains("goal")
        || lower.contains("objective")
        || lower.contains("problem")
        || lower.contains("friction")
        || lower.contains("motivation")
        || lower.contains("why")
    {
        AtomKind::FrictionPoint
    } else if lower.contains("validation")
        || lower.contains("success")
        || lower.contains("metric")
        || lower.contains("acceptance")
        || lower.contains("test criteria")
        || lower.contains("definition of done")
    {
        AtomKind::ValidationPoint
    } else if lower.contains("question")
        || lower.contains("unknown")
        || lower.contains("open item")
        || lower.contains("tbd")
        || lower.contains("to be determined")
    {
        AtomKind::OpenQuestion
    } else if lower.contains("decision")
        || lower.contains("chosen")
        || lower.contains("selected")
        || lower.contains("approach")
    {
        AtomKind::Decision
    } else if lower.contains("constraint")
        || lower.contains("requirement")
        || lower.contains("must")
        || lower.contains("non-goal")
    {
        AtomKind::Constraint
    } else {
        // Default: treat as a technical spec — agents get the full body.
        AtomKind::TechnicalSpec
    }
}

/// Extract atoms from a generic document section.
/// Subsections (### headings) produce individual atoms; otherwise the whole body is one atom.
fn extract_atoms_from_generic_section(section: &Section, kind: AtomKind) -> Vec<Atom> {
    // Check if the section has subsections — if so, each is its own atom.
    let has_subsections = section
        .body_lines
        .iter()
        .any(|l| matches!(parse_heading(l), Some((3, _))));

    if has_subsections {
        extract_subsection_atoms(section, kind)
    } else {
        let body = section.body_lines.join("\n").trim().to_string();
        if body.is_empty() {
            return Vec::new();
        }
        // Also extract list items as individual atoms when the body is a list
        // (common in design specs: "## Goals\n- goal A\n- goal B").
        let is_list = section
            .body_lines
            .iter()
            .any(|l| strip_list_marker(l.trim()).is_some());
        if is_list {
            extract_list_atoms(section, kind)
        } else {
            extract_block_atoms(section, kind)
        }
    }
}

// ── Shared extraction helpers ─────────────────────────────────────────────────

/// Extract a single atom from the entire section body.
fn extract_block_atoms(section: &Section, kind: AtomKind) -> Vec<Atom> {
    let body = section.body_lines.join("\n").trim().to_string();
    if body.is_empty() {
        return Vec::new();
    }

    let title = first_sentence(&body).unwrap_or_else(|| section.heading.to_string());
    let references = extract_references(&body);

    vec![Atom {
        kind,
        title,
        body,
        source_line: section.start_line,
        source_section: section.heading.to_string(),
        references,
    }]
}

/// Extract atoms from list items within a section.
fn extract_list_atoms(section: &Section, kind: AtomKind) -> Vec<Atom> {
    let mut atoms = Vec::new();
    let mut current_item: Option<(usize, String)> = None;

    for (offset, line) in section.body_lines.iter().enumerate() {
        let trimmed = line.trim();
        if let Some(text) = strip_list_marker(trimmed) {
            // Flush previous item
            if let Some((line_offset, item_text)) = current_item.take() {
                atoms.push(make_list_atom(
                    kind,
                    &item_text,
                    section.start_line + line_offset,
                    section.heading,
                ));
            }
            current_item = Some((offset, text.to_string()));
        } else if !trimmed.is_empty() {
            // Continuation line
            if let Some((_, ref mut text)) = current_item {
                text.push(' ');
                text.push_str(trimmed);
            }
        }
    }

    // Flush last item
    if let Some((line_offset, item_text)) = current_item {
        atoms.push(make_list_atom(
            kind,
            &item_text,
            section.start_line + line_offset,
            section.heading,
        ));
    }

    atoms
}

/// Extract atoms from subsections (### headings within a section).
fn extract_subsection_atoms(section: &Section, kind: AtomKind) -> Vec<Atom> {
    let mut atoms = Vec::new();
    let mut current_title: Option<String> = None;
    let mut current_body = String::new();
    let mut current_line = section.start_line;

    for (offset, line) in section.body_lines.iter().enumerate() {
        if let Some((_, heading)) = parse_heading(line) {
            // Flush previous subsection
            if let Some(title) = current_title.take() {
                let body = current_body.trim().to_string();
                if !body.is_empty() {
                    let references = extract_references(&body);
                    atoms.push(Atom {
                        kind,
                        title,
                        body,
                        source_line: current_line,
                        source_section: section.heading.to_string(),
                        references,
                    });
                }
            }
            current_title = Some(heading.to_string());
            current_body = String::new();
            current_line = section.start_line + offset;
        } else {
            current_body.push_str(line);
            current_body.push('\n');
        }
    }

    // Flush last subsection
    if let Some(title) = current_title {
        let body = current_body.trim().to_string();
        if !body.is_empty() {
            let references = extract_references(&body);
            atoms.push(Atom {
                kind,
                title,
                body,
                source_line: current_line,
                source_section: section.heading.to_string(),
                references,
            });
        }
    }

    // If no subsections found, treat whole body as one atom
    if atoms.is_empty() {
        return extract_block_atoms(section, kind);
    }

    atoms
}

fn make_list_atom(kind: AtomKind, text: &str, line: usize, section: &str) -> Atom {
    let references = extract_references(text);
    Atom {
        kind,
        title: first_sentence(text).unwrap_or_else(|| text.to_string()),
        body: text.to_string(),
        source_line: line,
        source_section: section.to_string(),
        references,
    }
}

// ── Frontmatter ───────────────────────────────────────────────────────────────

/// Extract YAML-style frontmatter from markdown.
/// Supports both `---` delimited YAML blocks and inline `**Key:** Value` patterns
/// common in ADRs (e.g., `**Status:** Proposed`, `**Depends on:** ADR-A`).
fn extract_frontmatter(markdown: &str) -> (DocMeta, &str) {
    let mut meta = DocMeta::default();

    // Try --- delimited YAML frontmatter first
    let trimmed = markdown.trim_start();
    if let Some(after_open) = trimmed.strip_prefix("---")
        && let Some(end) = after_open.find("\n---")
    {
        let yaml_block = &after_open[..end];
        meta = parse_frontmatter_lines(yaml_block);
        let remaining = &after_open[end + 4..]; // skip closing ---\n
        return (meta, remaining);
    }

    // Fall back to inline **Key:** Value patterns (scan until first ## heading)
    let lines: Vec<&str> = markdown.lines().collect();
    let mut body_start_line = 0;
    let mut frontmatter_lines = Vec::new();

    for (i, line) in lines.iter().enumerate() {
        let trimmed_line = line.trim();
        // Stop at first ## heading (the actual content)
        if trimmed_line.starts_with("## ") {
            body_start_line = i;
            break;
        }
        // Collect **Key:** Value lines
        if trimmed_line.starts_with("**") && trimmed_line.contains(":**") {
            frontmatter_lines.push(*line);
        }
        body_start_line = i + 1;
    }

    if !frontmatter_lines.is_empty() {
        let combined = frontmatter_lines.join("\n");
        meta = parse_inline_frontmatter(&combined);
    }

    // Find the byte offset of body_start_line
    let mut offset = 0;
    for (i, line) in markdown.lines().enumerate() {
        if i >= body_start_line {
            break;
        }
        offset += line.len() + 1; // +1 for newline
    }
    let body = if offset < markdown.len() {
        &markdown[offset..]
    } else {
        ""
    };

    (meta, body)
}

/// Parse --- delimited YAML frontmatter.
fn parse_frontmatter_lines(yaml: &str) -> DocMeta {
    let mut meta = DocMeta::default();
    for line in yaml.lines() {
        let trimmed = line.trim();
        if let Some((key, value)) = trimmed.split_once(':') {
            let key = key.trim().to_lowercase();
            let value = value.trim().to_string();
            if value.is_empty() {
                continue;
            }
            apply_meta_field(&mut meta, &key, &value);
        }
    }
    meta
}

/// Parse inline **Key:** Value frontmatter.
fn parse_inline_frontmatter(text: &str) -> DocMeta {
    let mut meta = DocMeta::default();
    for line in text.lines() {
        let trimmed = line.trim();
        // Pattern: **Key:** Value
        if let Some(rest) = trimmed.strip_prefix("**")
            && let Some((key_part, value)) = rest.split_once(":**")
        {
            let key = key_part.trim_end_matches('*').trim().to_lowercase();
            let value = value.trim().to_string();
            if !value.is_empty() {
                apply_meta_field(&mut meta, &key, &value);
            }
        }
    }
    meta
}

/// Apply a key-value pair to DocMeta.
fn apply_meta_field(meta: &mut DocMeta, key: &str, value: &str) {
    // Strip trailing backslash — some ADR files use `\` as a markdown soft-break
    // line continuation marker, which ends up in the parsed value.
    let value = value.trim_end_matches('\\').trim();
    match key {
        "status" => meta.status = Some(value.to_string()),
        "author" => meta.author = Some(value.to_string()),
        "date" => meta.date = Some(value.to_string()),
        "repo" => {
            // Strip parenthetical: "leyline (crates: ...)" → "leyline"
            let repo = value
                .split_once('(')
                .map(|(r, _)| r.trim())
                .unwrap_or(value);
            meta.repo = Some(repo.to_string());
        }
        k if k.starts_with("depends") => {
            // "depends on", "depends_on", "depends-on"
            meta.depends_on = parse_comma_or_ref_list(value);
        }
        k if k.starts_with("relates") => {
            // "relates to", "relates_to"
            meta.relates_to = parse_comma_or_ref_list(value);
        }
        _ => {} // ignore unknown keys
    }
}

/// Parse a comma-separated or space-separated list of references.
/// Handles: "ADR-A, ADR-B", "ADR-A (Sheaf Cache)", "ADR-A"
fn parse_comma_or_ref_list(value: &str) -> Vec<String> {
    value
        .split(',')
        .flat_map(|part| {
            let trimmed = part.trim();
            if trimmed.is_empty() {
                return None;
            }
            // Extract ID: take first word, strip parens
            let id = trimmed
                .split_whitespace()
                .next()
                .unwrap_or(trimmed)
                .trim_matches(|c: char| c == '(' || c == ')');
            Some(id.to_string())
        })
        .collect()
}

// ── Section extraction ────────────────────────────────────────────────────────

/// A parsed section from the markdown.
struct Section<'a> {
    heading: &'a str,
    #[allow(dead_code)]
    heading_level: usize,
    start_line: usize,
    body_lines: Vec<&'a str>,
}

/// Extract ## sections from markdown lines. ### subsections are included in their parent's body.
fn extract_sections<'a>(lines: &[&'a str]) -> Vec<Section<'a>> {
    let mut sections = Vec::new();
    let mut i = 0;

    // Find all ## headings (the main sections)
    while i < lines.len() {
        let line = lines[i];
        if let Some((level, heading)) = parse_heading(line)
            && level == 2
        {
            let start_line = i + 1;
            let mut body_lines = Vec::new();
            let mut j = i + 1;
            while j < lines.len() {
                if let Some((next_level, _)) = parse_heading(lines[j])
                    && next_level <= 2
                {
                    break;
                }
                body_lines.push(lines[j]);
                j += 1;
            }
            sections.push(Section {
                heading,
                heading_level: level,
                start_line,
                body_lines,
            });
            i = j;
            continue;
        }
        i += 1;
    }

    sections
}

/// Parse a markdown heading line, returning (level, text).
fn parse_heading(line: &str) -> Option<(usize, &str)> {
    let trimmed = line.trim();
    if !trimmed.starts_with('#') {
        return None;
    }
    let level = trimmed.chars().take_while(|&c| c == '#').count();
    if level > 6 {
        return None;
    }
    let text = trimmed[level..].trim();
    if text.is_empty() {
        return None;
    }
    Some((level, text))
}

/// Extract the first sentence from text (up to first period followed by space or end).
fn first_sentence(text: &str) -> Option<String> {
    let first_line = text.lines().next()?;
    let trimmed = first_line
        .trim()
        .trim_start_matches("**")
        .trim_end_matches("**");
    if trimmed.is_empty() {
        return None;
    }
    Some(trimmed.to_string())
}

/// Strip list markers (-, *, 1., 1)) from a line, returning the remainder.
fn strip_list_marker(line: &str) -> Option<&str> {
    let trimmed = line.trim();
    if let Some(rest) = trimmed.strip_prefix("- ") {
        return Some(rest);
    }
    if let Some(rest) = trimmed.strip_prefix("* ") {
        return Some(rest);
    }
    // Numbered lists: "1. " or "1) "
    if trimmed.chars().next().is_some_and(|c| c.is_ascii_digit()) {
        if let Some(pos) = trimmed.find(". ") {
            return Some(&trimmed[pos + 2..]);
        }
        if let Some(pos) = trimmed.find(") ") {
            return Some(&trimmed[pos + 2..]);
        }
    }
    None
}

/// Extract cross-references from markdown text.
/// Finds: [text](url), `backtick-refs`, and bead IDs (xxx-yyy pattern).
pub fn extract_references(text: &str) -> Vec<String> {
    let mut refs = Vec::new();

    // Markdown links: [text](url)
    let mut i = 0;
    let bytes = text.as_bytes();
    while i < bytes.len() {
        if bytes[i] == b'['
            && let Some(close_bracket) = text[i + 1..].find(']')
        {
            let after = i + 1 + close_bracket + 1;
            if after < bytes.len()
                && bytes[after] == b'('
                && let Some(close_paren) = text[after + 1..].find(')')
            {
                let url = &text[after + 1..after + 1 + close_paren];
                if !url.is_empty() {
                    refs.push(url.to_string());
                }
                i = after + 1 + close_paren + 1;
                continue;
            }
        }
        i += 1;
    }

    // Backtick references: `something`
    for cap in text.split('`').collect::<Vec<_>>().chunks(2) {
        if cap.len() == 2 && !cap[1].is_empty() && !cap[1].contains('\n') {
            let inner = cap[1].trim();
            if !inner.is_empty() && inner.len() < 100 {
                refs.push(inner.to_string());
            }
        }
    }

    refs.sort();
    refs.dedup();
    refs
}

/// Derive an ADR identifier from a file path.
/// "docs/adr/0007-bdr-enrichment.md" → "0007-bdr-enrichment"
/// "ADR-001.md" → "ADR-001"
fn adr_id_from_path(path: &str) -> String {
    std::path::Path::new(path)
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or(path)
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE_ADR: &str = r#"# ADR-001: Use Harmony Format

**Status:** Proposed
**Date:** 2026-03-14

## Context

ADRs exist across 60+ files but don't connect to actionable work.
Rosary tracks beads but lacks narrative coherence above threads.

## Decision

Use OpenAI's `Harmony` token format for a 3-tier lattice.

## Consequences

### Positive

- ADRs become actionable
- Cross-repo coherence via `mache-85t`

### Negative

- New dependency on `openai-harmony`
- Channel semantics overloaded

## Open Questions

1. Should `decade` be renamed?
2. Does StreamableParser work for non-LLM streams?
3. How to handle accretion conflicts?

## Implementation Plan

### Phase 1: Scaffold
- Create crate skeleton
- Add openai-harmony dependency

### Phase 2: Decompose
- Implement atom mapping
- Wire to Dolt

### Phase 3: Accrete
- Bead completion flows
- Mache schema

## References

- [Harmony format](https://developers.openai.com/cookbook/articles/openai-harmony)
- [openai-harmony crate](https://crates.io/crates/openai-harmony)
"#;

    const SAMPLE_DESIGN_SPEC: &str = r#"# X-Ray Design Spec

**Author:** James
**Date:** 2026-04-01

## Goals

- Provide coordinate-based embeddings without normalization
- Support orthogonal decomposition across model layers
- Enable unnormalized similarity metrics

## Implementation Plan

### Sub-Project A: Coordinate Transform
Build the core coordinate transform pipeline.
Uses `sheaf-cache` for memoization.

### Sub-Project B: Similarity Engine
Implement unnormalized dot-product similarity.

## Open Questions

- How does this interact with existing `mache` indexing?
- TBD: threshold selection for clustering

## Success Criteria

- `cargo test` passes with >95% coverage
- Latency < 100ms for single-community queries
"#;

    // ── ADR path tests ────────────────────────────────────────────────────────

    #[test]
    fn is_adr_shaped_detects_adr() {
        assert!(is_adr_shaped(SAMPLE_ADR));
    }

    #[test]
    fn is_adr_shaped_rejects_design_spec() {
        assert!(!is_adr_shaped(SAMPLE_DESIGN_SPEC));
    }

    #[test]
    fn parse_adr_produces_atoms() {
        let atoms = parse_adr(SAMPLE_ADR);
        assert!(!atoms.is_empty(), "should produce atoms from sample ADR");
    }

    #[test]
    fn context_produces_friction_point() {
        let atoms = parse_adr(SAMPLE_ADR);
        let friction: Vec<_> = atoms
            .iter()
            .filter(|a| a.kind == AtomKind::FrictionPoint)
            .collect();
        assert_eq!(friction.len(), 1);
        assert!(friction[0].body.contains("60+ files"));
    }

    #[test]
    fn decision_produces_decision_atom() {
        let atoms = parse_adr(SAMPLE_ADR);
        let decisions: Vec<_> = atoms
            .iter()
            .filter(|a| a.kind == AtomKind::Decision)
            .collect();
        assert_eq!(decisions.len(), 1);
        assert!(decisions[0].body.contains("Harmony"));
    }

    #[test]
    fn consequences_skipped_not_actionable() {
        let atoms = parse_adr(SAMPLE_ADR);
        let consequences: Vec<_> = atoms
            .iter()
            .filter(|a| a.kind == AtomKind::Consequence)
            .collect();
        assert_eq!(
            consequences.len(),
            0,
            "consequences are observations, not actionable work"
        );
    }

    #[test]
    fn open_questions_produce_atoms() {
        let atoms = parse_adr(SAMPLE_ADR);
        let questions: Vec<_> = atoms
            .iter()
            .filter(|a| a.kind == AtomKind::OpenQuestion)
            .collect();
        assert_eq!(questions.len(), 3);
    }

    #[test]
    fn implementation_phases_produce_atoms() {
        let atoms = parse_adr(SAMPLE_ADR);
        let phases: Vec<_> = atoms.iter().filter(|a| a.kind == AtomKind::Phase).collect();
        assert_eq!(phases.len(), 3);
        assert!(phases[0].title.contains("Scaffold"));
    }

    #[test]
    fn references_section_produces_no_atoms() {
        let atoms = parse_adr(SAMPLE_ADR);
        for atom in &atoms {
            assert_ne!(atom.source_section, "References");
        }
    }

    #[test]
    fn empty_markdown_produces_no_atoms() {
        let atoms = parse_adr("");
        assert!(atoms.is_empty());
    }

    #[test]
    fn non_adr_markdown_produces_no_atoms_via_adr_parser() {
        let atoms = parse_adr("# Hello World\n\nJust a regular doc.\n");
        // "Other" sections produce no atoms via ADR parser
        assert!(atoms.is_empty());
    }

    // ── Generic doc path tests ────────────────────────────────────────────────

    #[test]
    fn generic_doc_goals_produce_friction_points() {
        let parsed = parse_generic_doc(SAMPLE_DESIGN_SPEC);
        let friction: Vec<_> = parsed
            .atoms
            .iter()
            .filter(|a| a.kind == AtomKind::FrictionPoint)
            .collect();
        assert!(!friction.is_empty(), "Goals section → FrictionPoint atoms");
    }

    #[test]
    fn generic_doc_implementation_subsections_produce_phase_atoms() {
        let parsed = parse_generic_doc(SAMPLE_DESIGN_SPEC);
        let phases: Vec<_> = parsed
            .atoms
            .iter()
            .filter(|a| a.kind == AtomKind::Phase)
            .collect();
        assert_eq!(phases.len(), 2, "Sub-Project A and B → 2 Phase atoms");
        assert!(phases.iter().any(|a| a.title.contains("Sub-Project A")));
        assert!(phases.iter().any(|a| a.title.contains("Sub-Project B")));
    }

    #[test]
    fn generic_doc_open_questions_produce_atoms() {
        let parsed = parse_generic_doc(SAMPLE_DESIGN_SPEC);
        let questions: Vec<_> = parsed
            .atoms
            .iter()
            .filter(|a| a.kind == AtomKind::OpenQuestion)
            .collect();
        assert!(!questions.is_empty());
    }

    #[test]
    fn generic_doc_success_criteria_produce_validation_atoms() {
        let parsed = parse_generic_doc(SAMPLE_DESIGN_SPEC);
        let validation: Vec<_> = parsed
            .atoms
            .iter()
            .filter(|a| a.kind == AtomKind::ValidationPoint)
            .collect();
        assert!(!validation.is_empty());
    }

    // ── parse_doc_full routing ────────────────────────────────────────────────

    #[test]
    fn parse_doc_full_routes_adr_to_adr_parser() {
        let parsed = parse_doc_full(SAMPLE_ADR, "docs/adr/0001-harmony.md");
        // ADR path produces FrictionPoint from Context
        assert!(
            parsed
                .atoms
                .iter()
                .any(|a| a.kind == AtomKind::FrictionPoint)
        );
        // Provenance should be Adr
        assert!(matches!(
            parsed.meta.provenance,
            Some(ProvenanceRef::Adr { .. })
        ));
    }

    #[test]
    fn parse_doc_full_routes_generic_to_generic_parser() {
        let parsed = parse_doc_full(SAMPLE_DESIGN_SPEC, "docs/design/x-ray-spec.md");
        // Generic path produces Phase atoms from Implementation Plan
        assert!(parsed.atoms.iter().any(|a| a.kind == AtomKind::Phase));
        // Provenance should be Doc
        assert!(matches!(
            parsed.meta.provenance,
            Some(ProvenanceRef::Doc { .. })
        ));
    }

    #[test]
    fn parse_doc_full_sets_doc_path_in_provenance() {
        let parsed = parse_doc_full(SAMPLE_DESIGN_SPEC, "docs/design/x-ray-spec.md");
        if let Some(ProvenanceRef::Doc { path }) = &parsed.meta.provenance {
            assert_eq!(path, "docs/design/x-ray-spec.md");
        } else {
            panic!("expected Doc provenance");
        }
    }

    #[test]
    fn parse_doc_full_derives_adr_id_from_path() {
        let parsed = parse_doc_full(SAMPLE_ADR, "docs/adr/0007-bdr-enrichment.md");
        if let Some(ProvenanceRef::Adr { id }) = &parsed.meta.provenance {
            assert_eq!(id, "0007-bdr-enrichment");
        } else {
            panic!("expected Adr provenance with derived id");
        }
    }

    // ── Frontmatter tests ─────────────────────────────────────────────────────

    #[test]
    fn parse_adr_full_extracts_inline_status() {
        let parsed = parse_adr_full(SAMPLE_ADR);
        assert_eq!(parsed.meta.status.as_deref(), Some("Proposed"));
    }

    #[test]
    fn parse_adr_full_extracts_inline_date() {
        let parsed = parse_adr_full(SAMPLE_ADR);
        assert_eq!(parsed.meta.date.as_deref(), Some("2026-03-14"));
    }

    #[test]
    fn parse_yaml_frontmatter() {
        let adr = "---\nstatus: Proposed\nauthor: James\nrepo: leyline\ndepends on: ADR-A, ADR-B\nrelates to: ADR-C\n---\n\n## Context\n\nSome problem.\n";
        let parsed = parse_adr_full(adr);
        assert_eq!(parsed.meta.status.as_deref(), Some("Proposed"));
        assert_eq!(parsed.meta.author.as_deref(), Some("James"));
        assert_eq!(parsed.meta.repo.as_deref(), Some("leyline"));
        assert_eq!(parsed.meta.depends_on, vec!["ADR-A", "ADR-B"]);
        assert_eq!(parsed.meta.relates_to, vec!["ADR-C"]);
        assert!(!parsed.atoms.is_empty());
    }

    #[test]
    fn parse_adr_full_inline_depends_on() {
        let adr = "# ADR-B: Merkle Sync\n\n**Status:** Proposed\n**Depends on:** ADR-A (Sheaf Cache)\n**Relates to:** mache, leyline-net\n\n## Context\n\nSync is slow.\n";
        let parsed = parse_adr_full(adr);
        assert_eq!(parsed.meta.status.as_deref(), Some("Proposed"));
        assert_eq!(parsed.meta.depends_on, vec!["ADR-A"]);
        assert_eq!(parsed.meta.relates_to, vec!["mache", "leyline-net"]);
    }

    #[test]
    fn parse_adr_full_backward_compatible() {
        // parse_adr still works and returns same atoms
        let atoms = parse_adr(SAMPLE_ADR);
        let parsed = parse_adr_full(SAMPLE_ADR);
        assert_eq!(atoms.len(), parsed.atoms.len());
    }

    #[test]
    fn parse_adr_full_no_frontmatter() {
        let adr = "## Context\n\nJust a context section.\n";
        let parsed = parse_adr_full(adr);
        assert_eq!(parsed.meta.status, None);
        assert!(!parsed.atoms.is_empty());
    }

    #[test]
    fn doc_meta_serde_roundtrip() {
        let meta = DocMeta {
            status: Some("Proposed".into()),
            author: Some("James".into()),
            date: Some("2026-03-19".into()),
            repo: Some("leyline".into()),
            depends_on: vec!["ADR-A".into()],
            relates_to: vec!["ADR-C".into(), "mache".into()],
            provenance: Some(ProvenanceRef::Adr {
                id: "ADR-001".into(),
            }),
        };
        let json = serde_json::to_string(&meta).unwrap();
        let back: DocMeta = serde_json::from_str(&json).unwrap();
        assert_eq!(meta, back);
    }

    #[test]
    fn references_extracted_from_links() {
        let refs = extract_references("See [Harmony](https://example.com) for details");
        assert!(refs.contains(&"https://example.com".to_string()));
    }

    #[test]
    fn references_extracted_from_backticks() {
        let refs = extract_references("Uses `openai-harmony` and `mache-85t`");
        assert!(refs.contains(&"openai-harmony".to_string()));
        assert!(refs.contains(&"mache-85t".to_string()));
    }

    #[test]
    fn parse_heading_works() {
        assert_eq!(parse_heading("## Context"), Some((2, "Context")));
        assert_eq!(
            parse_heading("### Phase 1: Scaffold"),
            Some((3, "Phase 1: Scaffold"))
        );
        assert_eq!(parse_heading("not a heading"), None);
        assert_eq!(parse_heading("##"), None);
    }

    #[test]
    fn classify_adr_section_works() {
        assert_eq!(classify_adr_section("Context"), AdrSectionKind::Context);
        assert_eq!(
            classify_adr_section("Problem Statement"),
            AdrSectionKind::Context
        );
        assert_eq!(classify_adr_section("Decision"), AdrSectionKind::Decision);
        assert_eq!(
            classify_adr_section("Consequences"),
            AdrSectionKind::Consequences
        );
        assert_eq!(
            classify_adr_section("Open Questions"),
            AdrSectionKind::OpenQuestions
        );
        assert_eq!(
            classify_adr_section("Implementation Plan"),
            AdrSectionKind::Implementation
        );
        assert_eq!(
            classify_adr_section("Random Section"),
            AdrSectionKind::Other
        );
    }

    #[test]
    fn classify_generic_section_works() {
        assert_eq!(
            classify_generic_section("Implementation Plan"),
            AtomKind::Phase
        );
        assert_eq!(classify_generic_section("Sub-Project A"), AtomKind::Phase);
        assert_eq!(classify_generic_section("Goals"), AtomKind::FrictionPoint);
        assert_eq!(
            classify_generic_section("Success Criteria"),
            AtomKind::ValidationPoint
        );
        assert_eq!(
            classify_generic_section("Open Questions"),
            AtomKind::OpenQuestion
        );
        assert_eq!(
            classify_generic_section("Constraints"),
            AtomKind::Constraint
        );
        assert_eq!(
            classify_generic_section("Some Random Section"),
            AtomKind::TechnicalSpec
        );
    }

    #[test]
    fn adr_id_from_path_strips_extension() {
        assert_eq!(
            adr_id_from_path("docs/adr/0007-bdr-enrichment.md"),
            "0007-bdr-enrichment"
        );
        assert_eq!(adr_id_from_path("ADR-001.md"), "ADR-001");
    }
}
