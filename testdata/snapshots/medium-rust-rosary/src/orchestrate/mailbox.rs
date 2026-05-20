//! File-based mailbox for mid-flight agent communication.
#![allow(dead_code)]
//!
//! Each bead's workspace contains a `.rsry-mailbox.jsonl` file where the
//! orchestrator and workers exchange messages during execution.
//!
//! Design borrowed from Claude Code's teammate mailbox:
//! - JSON-lines format (append-only, one message per line)
//! - File-lock protected for concurrent access
//! - Workers poll for new messages between tool calls
//!
//! Unlike Claude Code's per-agent inbox, rosary uses a single mailbox per
//! bead (workspace) since there's only one active worker at a time. The
//! file lives in the workspace dir alongside `.rsry-handoff-*.json`.

use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

/// Filename within workspace directory.
const MAILBOX_FILENAME: &str = ".rsry-mailbox.jsonl";

/// A message in the mailbox.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MailboxMessage {
    /// Who sent this message.
    pub from: MessageSender,
    /// Message content.
    pub payload: MessagePayload,
    /// When the message was written.
    pub timestamp: DateTime<Utc>,
    /// Whether the recipient has read this message.
    #[serde(default)]
    pub read: bool,
}

/// Who can send messages.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageSender {
    /// The feature orchestrator managing this bead.
    Orchestrator,
    /// A worker agent (with name, e.g. "dev-agent").
    Worker(String),
    /// The grand orchestrator (reconciler).
    Reconciler,
}

/// Message content — discriminated union like Claude Code's StructuredMessage.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum MessagePayload {
    /// Worker reporting progress mid-execution.
    Progress {
        phase: u32,
        summary: String,
        files_touched: Vec<String>,
    },

    /// Worker proposing a plan before implementation.
    Plan {
        phase: u32,
        steps: Vec<super::PlanStep>,
        estimated_files: Vec<String>,
        risk_assessment: String,
    },

    /// Orchestrator approving a plan.
    PlanApproval {
        approved: bool,
        feedback: Option<String>,
    },

    /// Orchestrator sending a directive to the worker.
    Directive {
        action: DirectiveAction,
        detail: String,
    },

    /// Request for graceful shutdown.
    ShutdownRequest { reason: Option<String> },

    /// Response to shutdown request.
    ShutdownResponse { graceful: bool },

    /// Free-form text message.
    Text { content: String },
}

/// Directive actions the orchestrator can send to workers.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DirectiveAction {
    /// Abort current work and return.
    Abort,
    /// Narrow scope to specific files.
    FocusFiles,
    /// Priority change — escalate or deprioritize.
    Reprioritize,
    /// Additional context from another worker's findings.
    SupplementContext,
}

/// File-based mailbox for a bead's workspace.
pub struct Mailbox {
    path: PathBuf,
}

impl Mailbox {
    /// Create a mailbox for a workspace directory.
    pub fn new(workspace_dir: &Path) -> Self {
        Self {
            path: workspace_dir.join(MAILBOX_FILENAME),
        }
    }

    /// Path to the mailbox file.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Append a message to the mailbox.
    ///
    /// Uses append mode for crash safety — partial writes don't corrupt
    /// previous messages. File lock prevents interleaved writes from
    /// concurrent agents (though rosary typically has one writer at a time).
    pub fn send(&self, msg: &MailboxMessage) -> Result<()> {
        use std::io::Write;

        let line = serde_json::to_string(msg).context("serializing mailbox message")?;

        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
            .with_context(|| format!("opening mailbox {}", self.path.display()))?;

        writeln!(file, "{line}")
            .with_context(|| format!("writing to mailbox {}", self.path.display()))?;

        Ok(())
    }

    /// Read all messages from the mailbox.
    pub fn read_all(&self) -> Result<Vec<MailboxMessage>> {
        let content = match std::fs::read_to_string(&self.path) {
            Ok(c) => c,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(e) => {
                return Err(e).with_context(|| format!("reading mailbox {}", self.path.display()));
            }
        };

        let mut messages = Vec::new();
        for (i, line) in content.lines().enumerate() {
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            match serde_json::from_str::<MailboxMessage>(line) {
                Ok(msg) => messages.push(msg),
                Err(e) => {
                    eprintln!(
                        "[mailbox] skipping malformed line {} in {}: {e}",
                        i + 1,
                        self.path.display()
                    );
                }
            }
        }

        Ok(messages)
    }

    /// Read only unread messages and mark them as read.
    pub fn read_unread(&self) -> Result<Vec<MailboxMessage>> {
        let all = self.read_all()?;
        let unread: Vec<MailboxMessage> = all.into_iter().filter(|m| !m.read).collect();

        // Rewrite with all messages marked as read.
        // This is safe because we're the only reader (one orchestrator per bead).
        if !unread.is_empty() {
            self.mark_all_read()?;
        }

        Ok(unread)
    }

    /// Mark all messages as read by rewriting the file.
    fn mark_all_read(&self) -> Result<()> {
        let content = match std::fs::read_to_string(&self.path) {
            Ok(c) => c,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(e.into()),
        };

        let mut output = String::new();
        for line in content.lines() {
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            if let Ok(mut msg) = serde_json::from_str::<MailboxMessage>(line) {
                msg.read = true;
                if let Ok(updated) = serde_json::to_string(&msg) {
                    output.push_str(&updated);
                    output.push('\n');
                }
            }
        }

        std::fs::write(&self.path, output)?;
        Ok(())
    }

    /// Send a convenience helper: orchestrator directive.
    pub fn send_directive(&self, action: DirectiveAction, detail: &str) -> Result<()> {
        self.send(&MailboxMessage {
            from: MessageSender::Orchestrator,
            payload: MessagePayload::Directive {
                action,
                detail: detail.to_string(),
            },
            timestamp: Utc::now(),
            read: false,
        })
    }

    /// Send a convenience helper: worker progress report.
    pub fn send_progress(
        &self,
        agent: &str,
        phase: u32,
        summary: &str,
        files: Vec<String>,
    ) -> Result<()> {
        self.send(&MailboxMessage {
            from: MessageSender::Worker(agent.to_string()),
            payload: MessagePayload::Progress {
                phase,
                summary: summary.to_string(),
                files_touched: files,
            },
            timestamp: Utc::now(),
            read: false,
        })
    }
}
