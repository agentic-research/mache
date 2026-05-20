//! Session and code-provenance capture commands.
//!
//! - `rsry capture --from-session <path>`: transcript → BeadSpecs via LLM
//! - `rsry capture --from-code <repo> <path> [--symbol <sym>]`: code → BeadSpecs via LLM
//!
//! Both commands produce `BeadSpec[]` to stdout (dry-run default) or write to
//! `.beads/` with `--commit`.

use anyhow::Result;
use bdr::decompose::{BeadSpec, decompose_with_meta};
use bdr::parse::DocMeta;
use bdr::provenance::ProvenanceRef;
use std::path::Path;

// ── Session capture ───────────────────────────────────────────────────────────

/// Options for `rsry capture --from-session`.
pub struct SessionCaptureOpts<'a> {
    /// Transcript file path (use `-` for stdin).
    pub transcript_path: &'a str,
    /// LLM model shorthand: "haiku" (default), "sonnet", or a full model ID.
    pub model: &'a str,
}

/// Read the transcript, extract BDR atoms via LLM, and return BeadSpecs.
///
/// The provenance of every returned spec is `ProvenanceRef::Session`.
pub async fn capture_from_session(opts: &SessionCaptureOpts<'_>) -> Result<Vec<BeadSpec>> {
    let content = read_text_source(opts.transcript_path)?;
    let atoms = crate::bdr_enrich::extract_atoms_with_llm(&content, opts.model).await?;

    let provenance = ProvenanceRef::Session {
        transcript_path: opts.transcript_path.to_string(),
        summary: None,
    };

    let meta = DocMeta {
        provenance: Some(provenance),
        ..DocMeta::default()
    };

    let doc_id = stem(opts.transcript_path);
    Ok(decompose_with_meta(&atoms, &doc_id, &meta))
}

// ── Code provenance capture ───────────────────────────────────────────────────

/// Options for `rsry capture --from-code`.
pub struct CodeCaptureOpts<'a> {
    /// Repository short name (e.g. "rosary").
    pub repo: &'a str,
    /// File path relative to repo root (e.g. "src/bead.rs").
    pub path: &'a str,
    /// Optional symbol to scope the context (e.g. "BeadSpec").
    pub symbol: Option<&'a str>,
    /// LLM model shorthand: "haiku" (default), "sonnet", or a full model ID.
    pub model: &'a str,
    /// Filesystem root of the repo (for reading source files).
    pub repo_root: &'a Path,
}

/// Read code context, extract BDR atoms via LLM, and return BeadSpecs.
///
/// Reads the specified file (optionally filtered to the symbol's vicinity),
/// then calls the LLM to extract design atoms from the code. The provenance
/// of every returned spec is `ProvenanceRef::Code`.
pub async fn capture_from_code(opts: &CodeCaptureOpts<'_>) -> Result<Vec<BeadSpec>> {
    let context = build_code_context(opts)?;
    let atoms = crate::bdr_enrich::extract_atoms_with_llm(&context, opts.model).await?;

    let provenance = ProvenanceRef::Code {
        repo: opts.repo.to_string(),
        path: opts.path.to_string(),
        symbol: opts.symbol.map(str::to_string),
    };

    let meta = DocMeta {
        provenance: Some(provenance),
        repo: Some(opts.repo.to_string()),
        ..DocMeta::default()
    };

    let doc_id = opts.symbol.unwrap_or_else(|| {
        Path::new(opts.path)
            .file_stem()
            .and_then(|s| s.to_str())
            .unwrap_or(opts.path)
    });

    Ok(decompose_with_meta(&atoms, doc_id, &meta))
}

/// Build a code context string for the LLM from the file (optionally symbol-filtered).
fn build_code_context(opts: &CodeCaptureOpts<'_>) -> Result<String> {
    let file_path = opts.repo_root.join(opts.path);
    let source = std::fs::read_to_string(&file_path)
        .map_err(|e| anyhow::anyhow!("reading '{}': {e}", file_path.display()))?;

    let header = match opts.symbol {
        Some(sym) => format!(
            "// Code context: {}/{} (symbol: {})\n\n",
            opts.repo, opts.path, sym
        ),
        None => format!("// Code context: {}/{}\n\n", opts.repo, opts.path),
    };

    // When a symbol is specified, try to extract the relevant excerpt.
    // Falls back to the full file if the symbol isn't found.
    let content = match opts.symbol {
        Some(sym) => extract_symbol_context(&source, sym),
        None => source.clone(),
    };

    Ok(format!("{header}{content}"))
}

/// Extract lines around a symbol definition from source.
///
/// Returns a window of ±50 lines around the first occurrence of the symbol.
/// Falls back to the full source when the symbol is not found.
fn extract_symbol_context(source: &str, symbol: &str) -> String {
    let lines: Vec<&str> = source.lines().collect();
    let idx = lines.iter().position(|l| l.contains(symbol));

    match idx {
        Some(i) => {
            let start = i.saturating_sub(5);
            let end = (i + 50).min(lines.len());
            lines[start..end].join("\n")
        }
        None => source.to_string(),
    }
}

// ── Shared helpers ────────────────────────────────────────────────────────────

fn read_text_source(path: &str) -> Result<String> {
    if path == "-" {
        use std::io::Read;
        let mut buf = String::new();
        std::io::stdin()
            .read_to_string(&mut buf)
            .map_err(|e| anyhow::anyhow!("reading stdin: {e}"))?;
        return Ok(buf);
    }
    std::fs::read_to_string(path).map_err(|e| anyhow::anyhow!("reading '{}': {e}", path))
}

fn stem(path: &str) -> String {
    Path::new(path)
        .file_stem()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| "capture".to_string())
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod session {
    use super::*;
    use bdr::provenance::ProvenanceRef;

    #[test]
    fn read_transcript_from_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("transcript.md");
        std::fs::write(&path, "## Discussion\n\nWe decided to use Dolt.").unwrap();
        let content = read_text_source(path.to_str().unwrap()).unwrap();
        assert!(content.contains("Dolt"));
    }

    #[test]
    fn read_transcript_missing_file_errors() {
        let result = read_text_source("/nonexistent/path/transcript.md");
        assert!(result.is_err());
    }

    #[test]
    fn session_provenance_set_on_specs() {
        use bdr::atom::{Atom, AtomKind};

        let atoms = vec![Atom {
            kind: AtomKind::TechnicalSpec,
            title: "Use Dolt for bead storage".to_string(),
            body: "Dolt provides version-controlled SQL.".to_string(),
            source_line: 1,
            source_section: "Storage".to_string(),
            references: vec![],
        }];

        let provenance = ProvenanceRef::Session {
            transcript_path: "sessions/2026-04-29.md".to_string(),
            summary: None,
        };

        let meta = DocMeta {
            provenance: Some(provenance),
            ..DocMeta::default()
        };

        let specs = decompose_with_meta(&atoms, "session", &meta);
        assert_eq!(specs.len(), 1);

        let primary = specs[0].primary_source().unwrap();
        assert!(
            matches!(
                primary,
                ProvenanceRef::Session { transcript_path, .. }
                if transcript_path == "sessions/2026-04-29.md"
            ),
            "expected Session provenance, got {primary:?}"
        );
    }

    #[test]
    fn session_provenance_label() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/2026-04-29.md".to_string(),
            summary: Some("Discussed Dolt integration".to_string()),
        };
        assert_eq!(p.label(), "session:sessions/2026-04-29.md");
    }

    #[test]
    fn session_provenance_roundtrip() {
        let p = ProvenanceRef::Session {
            transcript_path: "sessions/foo.md".to_string(),
            summary: Some("design review".to_string()),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }
}

#[cfg(test)]
mod code {
    use super::*;
    use bdr::provenance::ProvenanceRef;

    #[test]
    fn code_provenance_roundtrip() {
        let p = ProvenanceRef::Code {
            repo: "rosary".to_string(),
            path: "src/bead.rs".to_string(),
            symbol: Some("BeadSpec".to_string()),
        };
        let json = serde_json::to_string(&p).unwrap();
        let back: ProvenanceRef = serde_json::from_str(&json).unwrap();
        assert_eq!(p, back);
    }

    #[test]
    fn code_provenance_label_with_symbol() {
        let p = ProvenanceRef::Code {
            repo: "rosary".to_string(),
            path: "src/bead.rs".to_string(),
            symbol: Some("BeadSpec".to_string()),
        };
        assert_eq!(p.label(), "code:rosary:src/bead.rs::BeadSpec");
    }

    #[test]
    fn code_provenance_label_without_symbol() {
        let p = ProvenanceRef::Code {
            repo: "rosary".to_string(),
            path: "src/bead.rs".to_string(),
            symbol: None,
        };
        assert_eq!(p.label(), "code:rosary:src/bead.rs");
    }

    #[test]
    fn build_code_context_reads_file() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("bead.rs");
        std::fs::write(&src, "pub struct BeadSpec { pub id: String }").unwrap();

        let opts = CodeCaptureOpts {
            repo: "rosary",
            path: "bead.rs",
            symbol: None,
            model: "haiku",
            repo_root: dir.path(),
        };

        let ctx = build_code_context(&opts).unwrap();
        assert!(ctx.contains("BeadSpec"));
        assert!(ctx.contains("Code context: rosary/bead.rs"));
    }

    #[test]
    fn build_code_context_with_symbol_filters_lines() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("bead.rs");
        let content = "// header\n".repeat(20)
            + "pub struct BeadSpec {\n    pub id: String,\n}\n"
            + "// footer\n".repeat(200).as_str();
        std::fs::write(&src, &content).unwrap();

        let opts = CodeCaptureOpts {
            repo: "rosary",
            path: "bead.rs",
            symbol: Some("BeadSpec"),
            model: "haiku",
            repo_root: dir.path(),
        };

        let ctx = build_code_context(&opts).unwrap();
        assert!(ctx.contains("BeadSpec"));
        // Should not include all 200 footer lines
        let lines: Vec<_> = ctx.lines().collect();
        assert!(
            lines.len() < 100,
            "context should be windowed, got {} lines",
            lines.len()
        );
    }

    #[test]
    fn build_code_context_missing_file_errors() {
        let dir = tempfile::tempdir().unwrap();
        let opts = CodeCaptureOpts {
            repo: "rosary",
            path: "nonexistent.rs",
            symbol: None,
            model: "haiku",
            repo_root: dir.path(),
        };
        assert!(build_code_context(&opts).is_err());
    }

    #[test]
    fn code_provenance_set_on_specs() {
        use bdr::atom::{Atom, AtomKind};

        let atoms = vec![Atom {
            kind: AtomKind::TechnicalSpec,
            title: "BeadSpec content_hash invariant".to_string(),
            body: "SHA-256 over title+description.".to_string(),
            source_line: 1,
            source_section: "struct".to_string(),
            references: vec![],
        }];

        let provenance = ProvenanceRef::Code {
            repo: "rosary".to_string(),
            path: "src/bead.rs".to_string(),
            symbol: Some("BeadSpec".to_string()),
        };

        let meta = DocMeta {
            provenance: Some(provenance),
            repo: Some("rosary".to_string()),
            ..DocMeta::default()
        };

        let specs = decompose_with_meta(&atoms, "BeadSpec", &meta);
        assert_eq!(specs.len(), 1);

        let primary = specs[0].primary_source().unwrap();
        assert!(
            matches!(
                primary,
                ProvenanceRef::Code { repo, path, symbol }
                if repo == "rosary" && path == "src/bead.rs" && symbol.as_deref() == Some("BeadSpec")
            ),
            "expected Code provenance, got {primary:?}"
        );
    }
}
