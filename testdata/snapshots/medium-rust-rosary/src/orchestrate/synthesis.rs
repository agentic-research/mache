//! Context synthesis between pipeline phases.
#![allow(dead_code)]
//!
//! The "never delegate understanding" pattern from Claude Code's coordinator mode.
//! Instead of forwarding raw handoff JSON to the next agent, the orchestrator:
//!
//! 1. Reads the previous worker's full output (transcript + handoff)
//! 2. Extracts key decisions, findings, and file changes
//! 3. Builds a contextualized prompt for the next worker
//!
//! This replaces the lossy handoff summary with rich, agent-aware context.

use crate::bead::Bead;
use crate::handoff::Handoff;

use super::ResearchFinding;
use super::transcript::TranscriptEntry;

/// Context gathered from previous phases, used to synthesize the next prompt.
pub struct SynthesisContext {
    /// The bead being worked on.
    pub bead: Bead,
    /// Full handoff chain from all previous phases.
    pub handoff_chain: Vec<Handoff>,
    /// Relevant transcript excerpts from the previous worker (fork-style context).
    /// Only populated when `fork_context = true` in config.
    pub transcript_excerpts: Vec<TranscriptEntry>,
    /// Research findings from fan-out phase (if any).
    pub research_findings: Vec<ResearchFinding>,
    /// The next agent that will receive this context.
    pub next_agent: String,
    /// The phase index for the next agent.
    pub next_phase: u32,
}

impl SynthesisContext {
    /// Build a synthesized prompt section for the next worker.
    ///
    /// This replaces `Handoff::format_for_prompt` when synthesis is enabled.
    /// Instead of just forwarding handoff metadata, it includes:
    /// - Relevant transcript excerpts (what the previous agent actually did/found)
    /// - Research findings (from parallel fan-out)
    /// - Specific file paths and line numbers (not just "files_changed")
    /// - Review hints contextualized to the next agent's role
    pub fn build_synthesized_prompt(&self) -> String {
        let mut sections = Vec::new();

        // Section 1: Research findings (if fan-out was used)
        if !self.research_findings.is_empty() {
            sections.push(self.format_research_findings());
        }

        // Section 2: Previous phase context (enhanced handoff)
        if !self.handoff_chain.is_empty() {
            sections.push(self.format_handoff_chain());
        }

        // Section 3: Transcript excerpts (fork-style context)
        if !self.transcript_excerpts.is_empty() {
            sections.push(self.format_transcript_excerpts());
        }

        // Section 4: Agent-specific guidance
        sections.push(self.format_agent_guidance());

        sections.join("\n\n")
    }

    /// Format research findings for the prompt.
    fn format_research_findings(&self) -> String {
        let mut out = String::from("## Research Findings\n\n");
        out.push_str("The following was discovered during the research phase:\n\n");

        for (i, finding) in self.research_findings.iter().enumerate() {
            out.push_str(&format!("### Finding {}\n\n", i + 1));
            out.push_str(&finding.summary);
            out.push('\n');

            if !finding.key_files.is_empty() {
                out.push_str("\nKey files:\n");
                for f in &finding.key_files {
                    out.push_str(&format!("- `{f}`\n"));
                }
            }

            if !finding.issues.is_empty() {
                out.push_str("\nIssues flagged:\n");
                for issue in &finding.issues {
                    out.push_str(&format!("- {issue}\n"));
                }
            }
        }

        out
    }

    /// Format the handoff chain — enhanced version of `Handoff::format_for_prompt`.
    fn format_handoff_chain(&self) -> String {
        let mut out = String::from("## Previous Phase Context\n\n");

        for handoff in &self.handoff_chain {
            out.push_str(&format!(
                "### Phase {} ({} via {})\n\n",
                handoff.phase, handoff.from_agent, handoff.provider
            ));
            out.push_str(&format!("**Summary**: {}\n\n", handoff.summary));

            if !handoff.files_changed.is_empty() {
                out.push_str("**Files changed**:\n");
                for f in &handoff.files_changed {
                    out.push_str(&format!("- `{f}`\n"));
                }
                out.push('\n');
            }

            if !handoff.commit_shas.is_empty() {
                out.push_str(&format!(
                    "**Commits**: {}\n\n",
                    handoff.commit_shas.join(", ")
                ));
            }

            if !handoff.review_hints.is_empty() {
                out.push_str("**Review hints**:\n");
                for hint in &handoff.review_hints {
                    out.push_str(&format!("- {hint}\n"));
                }
                out.push('\n');
            }

            if let Some(verdict) = &handoff.verdict {
                out.push_str(&format!("**Verdict**: {}\n", verdict.decision));
                if !verdict.concerns.is_empty() {
                    out.push_str("Concerns:\n");
                    for c in &verdict.concerns {
                        out.push_str(&format!("- {c}\n"));
                    }
                }
                if !verdict.suggestions.is_empty() {
                    out.push_str("Suggestions:\n");
                    for s in &verdict.suggestions {
                        out.push_str(&format!("- {s}\n"));
                    }
                }
                out.push('\n');
            }
        }

        out
    }

    /// Format transcript excerpts — the fork-style context sharing.
    ///
    /// Instead of giving the next agent a summary, give them the actual
    /// relevant observations and decisions the previous agent made.
    fn format_transcript_excerpts(&self) -> String {
        let mut out = String::from("## Previous Agent's Key Observations\n\n");
        out.push_str("The following are relevant excerpts from the previous agent's work:\n\n");

        for entry in &self.transcript_excerpts {
            match entry {
                TranscriptEntry::ToolUse { tool, summary, .. } => {
                    out.push_str(&format!("- **{tool}**: {summary}\n"));
                }
                TranscriptEntry::AssistantText { text, .. } => {
                    // Truncate long texts
                    let truncated = if text.len() > 500 {
                        format!("{}...", text.chars().take(500).collect::<String>())
                    } else {
                        text.clone()
                    };
                    out.push_str(&format!("- **Agent observation**: {truncated}\n"));
                }
                TranscriptEntry::Decision { description, .. } => {
                    out.push_str(&format!("- **Decision**: {description}\n"));
                }
            }
        }

        out
    }

    /// Generate agent-specific guidance based on the next agent's role.
    fn format_agent_guidance(&self) -> String {
        let mut out = String::from("## Your Task\n\n");

        match self.next_agent.as_str() {
            "dev-agent" => {
                out.push_str(
                    "You are implementing changes based on the research and context above. \
                     Focus on the specific files and issues identified. Commit your changes \
                     with a clear message referencing the bead ID.\n",
                );
            }
            "staging-agent" => {
                out.push_str(
                    "You are reviewing the implementation above for test validity. \
                     Your core question: 'If I deleted the production code and replaced it \
                     with a no-op, would these tests fail?' Focus on the files changed and \
                     the review hints.\n",
                );
            }
            "prod-agent" => {
                out.push_str(
                    "You are reviewing for production safety at the module level. \
                     Look for: resource leaks, error swallowing, concurrency bugs, \
                     data loss vectors, and performance anti-patterns in the changed files.\n",
                );
            }
            "architect-agent" => {
                out.push_str(
                    "You are reviewing the system architecture. Consider dependency graphs, \
                     module boundaries, and whether the changes maintain or improve the \
                     overall system structure.\n",
                );
            }
            _ => {
                out.push_str("Review the context above and proceed with your assigned task.\n");
            }
        }

        out
    }
}
