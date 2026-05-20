//! Feature Orchestrator — per-bead lifecycle coordinator.
//!
//! Sits between the grand orchestrator (reconciler) and worker agents.
//! Each `FeatureOrchestrator` owns one bead's full lifecycle:
//!
//! 1. **Research fan-out** — spawn parallel read-only workers
//! 2. **Synthesis** — read worker output, build contextualized prompts
//! 3. **Implementation** — dispatch dev-agent with synthesized context
//! 4. **Verification** — staging-agent review, prod-agent safety check
//! 5. **Course correction** — mid-flight messaging, session continuation
//!
//! Inspired by Claude Code's coordinator mode and agent teams architecture.
//! Key principle: "never delegate understanding" — the orchestrator reads
//! worker output and synthesizes before dispatching the next phase.
//!
//! ## Architecture
//!
//! ```text
//! Grand Orchestrator (reconciler)
//!   ├─ FeatureOrchestrator A (bead X)
//!   │    ├─ research workers (parallel, ReadOnly)
//!   │    ├─ synthesis step (reads output, builds prompt)
//!   │    ├─ dev-agent (Implement)
//!   │    ├─ staging-agent (ReadOnly, adversarial)
//!   │    └─ mailbox (mid-flight communication)
//!   ├─ FeatureOrchestrator B (bead Y)
//!   │    └─ ...
//!   └─ ...
//! ```
//!
//! Enabled via `[orchestration] mode = "hierarchical"` in config.
//! Default `mode = "flat"` preserves existing reconciler behavior.

pub mod fanout;
pub mod mailbox;
pub mod plan_gate;
pub mod synthesis;
pub mod transcript;

#[cfg(test)]
mod tests;

use std::path::PathBuf;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

use crate::dispatch::AgentHandle;
use crate::handoff::Handoff;
use crate::store::WorkRef;

// ── Orchestrator State Machine ──────────────────────────────────

/// Current state of a feature orchestrator's lifecycle.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum OrchestratorState {
    /// Waiting to start — just created, no workers spawned yet.
    Idle,

    /// Parallel research phase — multiple read-only workers exploring
    /// the codebase to gather context before implementation.
    ResearchFanOut {
        /// Number of workers spawned.
        worker_count: usize,
        /// Number completed so far.
        completed: usize,
        /// Summaries from completed workers.
        findings: Vec<ResearchFinding>,
    },

    /// Synthesizing research findings into an implementation prompt.
    /// This is the "never delegate understanding" step — the orchestrator
    /// reads all worker output and builds a contextualized prompt.
    Synthesizing,

    /// Waiting for plan approval before implementation.
    /// Worker proposed a plan; orchestrator validates scope/risk.
    PlanApproval { plan: WorkerPlan },

    /// A worker agent is currently executing (dev, staging, prod, etc.).
    AwaitingWorker { agent: String, phase: u32 },

    /// Pipeline completed successfully — bead ready to close.
    Terminal,

    /// Pipeline failed — max retries exceeded or unrecoverable error.
    Failed { retries: u32, reason: String },
}

/// A finding from a research worker.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResearchFinding {
    /// Which research query this answers.
    pub query_index: usize,
    /// Human-readable summary of findings.
    pub summary: String,
    /// Key files discovered.
    pub key_files: Vec<String>,
    /// Issues or concerns flagged.
    pub issues: Vec<String>,
}

/// A worker's proposed implementation plan.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerPlan {
    pub steps: Vec<PlanStep>,
    pub estimated_files: Vec<String>,
    pub risk_assessment: String,
}

/// A single step in a worker's plan.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PlanStep {
    pub description: String,
    pub files: Vec<String>,
    pub risk: PlanStepRisk,
}

/// Risk level for a plan step.
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PlanStepRisk {
    Low,
    Medium,
    High,
}

// ── Continue vs Spawn Decision ──────────────────────────────────

/// Decision about whether to continue an existing worker session
/// or spawn a fresh one for the next phase.
///
/// Modeled after Claude Code's explicit decision matrix:
/// - High context overlap → continue (reuse session)
/// - Low context overlap → spawn fresh
/// - Correcting a failure → continue (has error context)
/// - Verification after impl → spawn fresh (needs fresh eyes)
#[derive(Debug, Clone, Copy)]
pub enum SessionDecision {
    /// Continue the existing session (pass session_id to --resume).
    Continue,
    /// Spawn a fresh agent (no prior context).
    SpawnFresh,
}

// ── Feature Orchestrator ────────────────────────────────────────

/// Per-bead lifecycle coordinator.
///
/// Runs as an async tokio task, managing one bead through its full
/// pipeline: research → synthesis → implementation → verification.
///
/// Unlike the flat reconciler which fire-and-forgets agents, the
/// feature orchestrator can:
/// - Fan out parallel research workers
/// - Synthesize between phases (read worker output, build prompt)
/// - Send mid-flight messages via mailbox
/// - Decide continue-vs-spawn for session reuse
/// - Apply plan-mode approval gates
#[allow(dead_code)]
pub struct FeatureOrchestrator {
    /// The bead this orchestrator owns.
    pub bead_ref: WorkRef,
    /// Issue type (captured at creation for pipeline lookup).
    pub issue_type: String,
    /// Agent sequence from PipelineEngine.
    pub pipeline: Vec<String>,
    /// Current phase index into the pipeline.
    pub current_phase: usize,
    /// Orchestrator lifecycle state.
    pub state: OrchestratorState,
    /// Workspace directory for this bead.
    pub work_dir: PathBuf,
    /// Mailbox for mid-flight communication.
    pub mailbox: mailbox::Mailbox,
    /// Handle to the currently-running worker (if any).
    pub worker_handle: Option<AgentHandle>,
    /// Handoff chain from previous phases.
    pub handoff_chain: Vec<Handoff>,
    /// Transcript excerpts from previous workers (fork-style context).
    pub transcript_cache: Vec<transcript::TranscriptEntry>,
    /// Last worker's session ID (for continue-vs-spawn decision).
    pub last_session_id: Option<String>,
    /// Agent that most recently completed work (for session_decision from_agent).
    /// Saved before incrementing current_phase so session_decision can compare
    /// the outgoing agent against the incoming one without an off-by-one error.
    pub last_completed_agent: Option<String>,
    /// Retry count for the current phase.
    pub retries: u32,
    /// When this orchestrator was created.
    pub created_at: DateTime<Utc>,
    /// Configuration for orchestrator behavior.
    pub config: OrchestratorBehavior,
}

/// Per-orchestrator behavior flags (from global OrchestrationConfig).
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct OrchestratorBehavior {
    /// Enable synthesis LLM call between phases.
    pub synthesis: bool,
    /// Enable parallel research fan-out for scoping phase.
    pub fan_out: bool,
    /// Enable plan-mode approval gate before implementation.
    pub plan_gate: bool,
    /// Max parallel research workers.
    pub max_research_workers: usize,
    /// Pass transcript excerpts (fork-style) instead of just handoff JSON.
    pub fork_context: bool,
}

impl Default for OrchestratorBehavior {
    fn default() -> Self {
        Self {
            synthesis: true,
            fan_out: false,
            plan_gate: false,
            max_research_workers: 3,
            fork_context: true,
        }
    }
}

/// Outcome of a single orchestrator tick — tells the reconciler what to do.
#[derive(Debug)]
pub enum TickOutcome {
    /// No state change — orchestrator is waiting for something.
    Idle,
    /// Orchestrator needs a worker spawned.
    /// The reconciler should call `dispatch::spawn()` and hand the AgentHandle back.
    NeedsSpawn {
        agent: String,
        phase: u32,
        /// Whether to resume an existing session or start fresh.
        session_decision: SessionDecision,
    },
    /// Current worker completed. Orchestrator provides the exit status.
    WorkerCompleted { exit_success: bool },
    /// Orchestrator wants to advance to next pipeline phase.
    /// The reconciler should verify, then call `advance()`.
    #[allow(dead_code)]
    ReadyToAdvance { next_agent: String },
    /// Pipeline is done — bead can be closed/PR'd.
    Terminal,
    /// Pipeline failed permanently.
    Failed { reason: String },
}

impl FeatureOrchestrator {
    /// Create a new orchestrator for a bead.
    pub fn new(
        bead_ref: WorkRef,
        issue_type: String,
        pipeline: Vec<String>,
        work_dir: PathBuf,
        config: OrchestratorBehavior,
    ) -> Self {
        let mailbox = mailbox::Mailbox::new(&work_dir);
        Self {
            bead_ref,
            issue_type,
            pipeline,
            current_phase: 0,
            state: OrchestratorState::Idle,
            work_dir,
            mailbox,
            worker_handle: None,
            handoff_chain: Vec::new(),
            transcript_cache: Vec::new(),
            last_session_id: None,
            last_completed_agent: None,
            retries: 0,
            created_at: Utc::now(),
            config,
        }
    }

    /// Drive the orchestrator state machine forward one step.
    ///
    /// Called by the reconciler each iteration. Returns what action (if any)
    /// the reconciler should take. This keeps the orchestrator deterministic —
    /// it never spawns agents itself, it just tells the reconciler what it needs.
    pub fn tick(&mut self) -> TickOutcome {
        match &self.state {
            OrchestratorState::Idle => {
                // Emit NeedsSpawn for the current pipeline phase.
                // On the very first tick this is phase 0; after on_worker_completed
                // advances current_phase and resets state to Idle, it's the next phase.
                if let Some(agent) = self.pipeline.get(self.current_phase).cloned() {
                    let decision = self.session_decision(&agent);
                    self.state = OrchestratorState::AwaitingWorker {
                        agent: agent.clone(),
                        phase: self.current_phase as u32,
                    };
                    TickOutcome::NeedsSpawn {
                        agent,
                        phase: self.current_phase as u32,
                        session_decision: decision,
                    }
                } else {
                    self.state = OrchestratorState::Terminal;
                    TickOutcome::Terminal
                }
            }

            OrchestratorState::AwaitingWorker { .. } => {
                // Check if worker has completed.
                if let Some(ref mut handle) = self.worker_handle {
                    match handle.try_wait() {
                        Ok(Some(success)) => {
                            // Worker done — take the handle out.
                            let _handle = self.worker_handle.take();
                            TickOutcome::WorkerCompleted {
                                exit_success: success,
                            }
                        }
                        Ok(None) => {
                            // Still running — check for hard timeout.
                            if handle.elapsed() > chrono::Duration::hours(4) {
                                eprintln!(
                                    "[orchestrator] {} killing worker (4h timeout)",
                                    self.bead_ref.bead_id
                                );
                                let _ = handle.kill();
                                let _handle = self.worker_handle.take();
                                TickOutcome::WorkerCompleted {
                                    exit_success: false,
                                }
                            } else {
                                TickOutcome::Idle
                            }
                        }
                        Err(e) => {
                            eprintln!("[orchestrator] {} poll error: {e}", self.bead_ref.bead_id);
                            let _handle = self.worker_handle.take();
                            TickOutcome::WorkerCompleted {
                                exit_success: false,
                            }
                        }
                    }
                } else {
                    // Handle is owned by the reconciler (self.active) — just wait.
                    // The reconciler calls on_worker_completed() when the handle exits.
                    TickOutcome::Idle
                }
            }

            OrchestratorState::Terminal => TickOutcome::Terminal,

            OrchestratorState::Failed { reason, .. } => TickOutcome::Failed {
                reason: reason.clone(),
            },

            // Synthesis, PlanApproval, ResearchFanOut — these will be
            // implemented in later phases. For now, skip straight through.
            OrchestratorState::Synthesizing => {
                // Synthesis not yet wired. current_phase was already advanced by
                // on_worker_completed before entering this state — reset to Idle
                // so the next tick() emits NeedsSpawn for the current phase.
                // (Setting AwaitingWorker here causes a recursive tick() on a state
                // with no handle, which returns Idle — the pipeline deadlocks.)
                if self.current_phase < self.pipeline.len() {
                    self.state = OrchestratorState::Idle;
                } else {
                    self.state = OrchestratorState::Terminal;
                }
                self.tick()
            }
            OrchestratorState::PlanApproval { .. } => {
                // Auto-approve for now.
                self.advance_to_next_phase();
                self.tick()
            }
            OrchestratorState::ResearchFanOut { .. } => {
                // Fan-out not yet wired — skip to first pipeline agent.
                self.advance_to_next_phase();
                self.tick()
            }
        }
    }

    /// Record that a worker completed and decide the next step.
    ///
    /// Called by the reconciler after verification. If verification passed and
    /// there's a next agent, transitions to the appropriate state. If this was
    /// the last agent, moves to Terminal.
    pub fn on_worker_completed(&mut self, passed: bool, max_retries: u32) {
        if passed {
            self.retries = 0;
            // Bug 3 fix: save current agent BEFORE advancing current_phase.
            // session_decision() uses last_completed_agent as "from_agent" —
            // if we read pipeline[current_phase] after incrementing, it gives
            // the next agent, causing every non-retry transition to look like
            // a same-agent retry and return Continue instead of SpawnFresh.
            self.last_completed_agent = self.pipeline.get(self.current_phase).cloned();
            let next_phase = self.current_phase + 1;
            if next_phase < self.pipeline.len() {
                self.current_phase = next_phase;
                if self.config.synthesis {
                    self.state = OrchestratorState::Synthesizing;
                } else {
                    // Bug 1 fix: set Idle instead of AwaitingWorker.
                    // AwaitingWorker with no handle → tick() returns Idle → pipeline
                    // deadlocks. Idle → tick() emits NeedsSpawn for current_phase.
                    self.state = OrchestratorState::Idle;
                }
            } else {
                self.state = OrchestratorState::Terminal;
            }
        } else {
            self.retries += 1;
            if self.retries >= max_retries {
                self.state = OrchestratorState::Failed {
                    retries: self.retries,
                    reason: format!(
                        "max retries ({}) exceeded at phase {}",
                        max_retries, self.current_phase
                    ),
                };
            } else {
                // Retry same phase — record the failing agent for session_decision,
                // then reset to Idle so tick() emits NeedsSpawn (consistent with the
                // success path; AwaitingWorker with no handle returns Idle and deadlocks).
                self.last_completed_agent = self.pipeline.get(self.current_phase).cloned();
                self.state = OrchestratorState::Idle;
                self.worker_handle = None;
            }
        }
    }

    /// Give the orchestrator a spawned worker handle.
    #[allow(dead_code)]
    pub fn set_worker_handle(&mut self, handle: AgentHandle) {
        self.worker_handle = Some(handle);
    }

    /// Get the current agent name (if in AwaitingWorker state).
    pub fn current_agent(&self) -> Option<&str> {
        match &self.state {
            OrchestratorState::AwaitingWorker { agent, .. } => Some(agent.as_str()),
            _ => None,
        }
    }

    /// Advance to the next pipeline phase (helper for synthesis/plan_gate skip).
    fn advance_to_next_phase(&mut self) {
        let next = self.current_phase + 1;
        if next < self.pipeline.len() {
            self.current_phase = next;
            let agent = self.pipeline[next].clone();
            self.state = OrchestratorState::AwaitingWorker {
                agent,
                phase: next as u32,
            };
        } else {
            self.state = OrchestratorState::Terminal;
        }
    }

    /// Decide whether to continue an existing worker session or spawn fresh.
    ///
    /// Based on Claude Code's decision matrix:
    /// - Research explored the files to edit → Continue
    /// - Research was broad, implementation narrow → Spawn fresh
    /// - Correcting a failure → Continue (has error context)
    /// - Verification after implementation → Spawn fresh (fresh eyes)
    /// - Wrong approach entirely → Spawn fresh (avoid anchoring)
    pub fn session_decision(&self, next_agent: &str) -> SessionDecision {
        // No previous session → must spawn fresh
        if self.last_session_id.is_none() {
            return SessionDecision::SpawnFresh;
        }

        // Bug 3 fix: use last_completed_agent (saved before current_phase was
        // incremented) as the "from" agent. Reading pipeline[current_phase] here
        // returns the NEXT agent (already incremented), which caused every
        // non-retry advancement to match the "same-agent retry" arm.
        let from_agent = self.last_completed_agent.as_deref().unwrap_or("");

        match (from_agent, next_agent) {
            // Verification should always get fresh eyes
            (_, "staging-agent" | "prod-agent" | "skeptic-agent") => SessionDecision::SpawnFresh,

            // Retrying the same agent → continue (has error context)
            (current, next) if current == next => SessionDecision::Continue,

            // Scoping → dev: research explored the files, dev needs that context
            ("scoping-agent", "dev-agent") => SessionDecision::Continue,

            // Default: spawn fresh for unrelated agents
            _ => SessionDecision::SpawnFresh,
        }
    }
}

// ── Persistent State ────────────────────────────────────────────

/// Persistent record of a feature orchestrator's state.
/// Stored in the backend for crash recovery.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OrchestratorRecord {
    pub bead_ref: WorkRef,
    pub issue_type: String,
    pub state: OrchestratorState,
    pub current_phase: u32,
    pub current_agent: Option<String>,
    pub retries: u32,
    pub last_session_id: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}
