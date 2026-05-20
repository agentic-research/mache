//! Parse `.rsry-stream.jsonl` for fork-style context sharing.
#![allow(dead_code)]
//!
//! Claude Code's fork subagent pattern: instead of summarizing what a worker did,
//! pass the relevant parts of their actual transcript to the next worker.
//! This gives the next agent concrete observations, not lossy summaries.
//!
//! We parse the JSONL stream log that Claude/Gemini write during execution,
//! extracting tool uses, assistant text, and key decisions.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::path::Path;

/// A parsed entry from an agent's stream log.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum TranscriptEntry {
    /// A tool invocation with its result summarized.
    ToolUse {
        tool: String,
        summary: String,
        /// Files involved (from tool input, if applicable).
        files: Vec<String>,
    },

    /// Assistant text response (reasoning, observations).
    AssistantText {
        text: String,
        /// Whether this was a "thinking" block vs. visible text.
        is_thinking: bool,
    },

    /// A discrete decision point (extracted from assistant reasoning).
    Decision { description: String },
}

/// Raw JSONL event from Claude's `--output-format json`.
///
/// We only parse the fields we care about — the full format has many
/// more fields (tokens, model, stop_reason, etc.).
#[derive(Debug, Deserialize)]
struct StreamEvent {
    #[serde(default)]
    r#type: String,
    /// For "assistant" events: the content blocks.
    #[serde(default)]
    message: Option<AssistantMessage>,
}

#[derive(Debug, Deserialize)]
struct AssistantMessage {
    #[serde(default)]
    content: Vec<ContentBlock>,
}

#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ContentBlock {
    Text {
        text: String,
    },
    ToolUse {
        name: String,
        input: serde_json::Value,
    },
    ToolResult {
        content: Option<serde_json::Value>,
        #[serde(default)]
        tool_use_id: String,
    },
    Thinking {
        thinking: String,
    },
    #[serde(other)]
    Unknown,
}

/// Parse a `.rsry-stream.jsonl` file into transcript entries.
///
/// Extracts the most relevant entries for context sharing:
/// - Tool uses with file paths
/// - Assistant observations (non-trivial text blocks)
/// - Thinking blocks that contain decisions
///
/// Returns entries in chronological order, limited to `max_entries`
/// to avoid bloating the next agent's context.
pub fn parse_transcript(stream_path: &Path, max_entries: usize) -> Result<Vec<TranscriptEntry>> {
    let content = std::fs::read_to_string(stream_path)
        .with_context(|| format!("reading stream log {}", stream_path.display()))?;

    let mut entries = Vec::new();

    for line in content.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }

        let event: StreamEvent = match serde_json::from_str(line) {
            Ok(e) => e,
            Err(_) => continue, // Skip malformed lines
        };

        if event.r#type != "assistant" {
            continue;
        }

        let Some(message) = event.message else {
            continue;
        };

        for block in message.content {
            match block {
                ContentBlock::ToolUse { name, input } => {
                    let files = extract_files_from_input(&input);
                    let summary = summarize_tool_use(&name, &input);
                    entries.push(TranscriptEntry::ToolUse {
                        tool: name,
                        summary,
                        files,
                    });
                }
                ContentBlock::Text { text } => {
                    // Skip trivial responses
                    if text.len() > 50 {
                        entries.push(TranscriptEntry::AssistantText {
                            text,
                            is_thinking: false,
                        });
                    }
                }
                ContentBlock::Thinking { thinking } => {
                    // Extract decision points from thinking
                    if let Some(decision) = extract_decision(&thinking) {
                        entries.push(TranscriptEntry::Decision {
                            description: decision,
                        });
                    }
                }
                _ => {}
            }
        }
    }

    // Keep only the most recent entries to avoid context bloat
    if entries.len() > max_entries {
        entries = entries.split_off(entries.len() - max_entries);
    }

    Ok(entries)
}

/// Extract file paths from tool input JSON.
fn extract_files_from_input(input: &serde_json::Value) -> Vec<String> {
    let mut files = Vec::new();

    // Common patterns: file_path, path, file, paths
    for key in &["file_path", "path", "file", "pattern"] {
        if let Some(val) = input.get(key).and_then(|v| v.as_str()) {
            files.push(val.to_string());
        }
    }
    if let Some(arr) = input.get("paths").and_then(|v| v.as_array()) {
        for v in arr {
            if let Some(s) = v.as_str() {
                files.push(s.to_string());
            }
        }
    }

    files
}

/// Summarize a tool use into a one-line description.
fn summarize_tool_use(tool: &str, input: &serde_json::Value) -> String {
    match tool {
        "Read" | "FileRead" => {
            let path = input
                .get("file_path")
                .and_then(|v| v.as_str())
                .unwrap_or("unknown");
            format!("Read {path}")
        }
        "Edit" | "FileEdit" => {
            let path = input
                .get("file_path")
                .and_then(|v| v.as_str())
                .unwrap_or("unknown");
            format!("Edited {path}")
        }
        "Write" | "FileWrite" => {
            let path = input
                .get("file_path")
                .and_then(|v| v.as_str())
                .unwrap_or("unknown");
            format!("Wrote {path}")
        }
        "Bash" => {
            let cmd = input
                .get("command")
                .and_then(|v| v.as_str())
                .unwrap_or("unknown");
            let truncated = if cmd.len() > 80 {
                format!("{}...", cmd.chars().take(80).collect::<String>())
            } else {
                cmd.to_string()
            };
            format!("Ran: {truncated}")
        }
        "Grep" | "Glob" => {
            let pattern = input.get("pattern").and_then(|v| v.as_str()).unwrap_or("?");
            format!("{tool}({pattern})")
        }
        _ => {
            // MCP tools or unknown — just name them
            format!("{tool}()")
        }
    }
}

/// Extract decision points from thinking blocks.
///
/// Looks for patterns like "I'll...", "The approach is...", "This means..."
/// that indicate the agent made a discrete decision.
fn extract_decision(thinking: &str) -> Option<String> {
    // Simple heuristic: look for decision-indicating phrases
    let decision_markers = [
        "I'll ",
        "I will ",
        "The approach ",
        "The fix ",
        "The solution ",
        "This means ",
        "So the plan ",
        "The root cause ",
    ];

    for line in thinking.lines() {
        let trimmed = line.trim();
        for marker in &decision_markers {
            if trimmed.starts_with(marker) && trimmed.len() > 30 {
                let truncated = if trimmed.len() > 200 {
                    format!("{}...", &trimmed[..200])
                } else {
                    trimmed.to_string()
                };
                return Some(truncated);
            }
        }
    }

    None
}
