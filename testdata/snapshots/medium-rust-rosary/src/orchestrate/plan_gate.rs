//! Plan-mode approval gate.
#![allow(dead_code)]
//!
//! Before a worker starts implementing, it can propose a plan that the
//! orchestrator validates. This prevents the "agent rewrites half the
//! codebase" failure mode.
//!
//! Modeled after Claude Code's `planModeRequired` flag where teammates
//! must present a plan and get leader approval before writing code.
//!
//! ## Flow
//!
//! 1. Worker sends a `Plan` message to the mailbox
//! 2. Orchestrator validates: scope, risk, file overlap with other beads
//! 3. Orchestrator responds with `PlanApproval` (approved/revised/rejected)
//! 4. Worker proceeds with approved plan or revises

use super::{PlanStep, PlanStepRisk, WorkerPlan};

/// Result of plan validation.
#[derive(Debug, Clone)]
pub enum PlanValidation {
    /// Plan is acceptable — proceed with implementation.
    Approved,
    /// Plan needs revision — provide feedback.
    NeedsRevision { feedback: String },
    /// Plan is rejected — too risky or out of scope.
    Rejected { reason: String },
}

/// Validate a worker's proposed plan.
///
/// Checks:
/// - Total file count doesn't exceed threshold
/// - No high-risk steps in low-priority beads
/// - Files don't overlap with other active beads (requires active bead list)
pub fn validate_plan(
    plan: &WorkerPlan,
    max_files: usize,
    active_files: &[String],
) -> PlanValidation {
    // Check file count
    if plan.estimated_files.len() > max_files {
        return PlanValidation::NeedsRevision {
            feedback: format!(
                "Plan touches {} files, which exceeds the limit of {}. \
                 Please narrow the scope.",
                plan.estimated_files.len(),
                max_files
            ),
        };
    }

    // Check for file overlap with other active beads
    let overlapping: Vec<&String> = plan
        .estimated_files
        .iter()
        .filter(|f| active_files.contains(f))
        .collect();

    if !overlapping.is_empty() {
        let files_str = overlapping
            .iter()
            .map(|f| format!("`{f}`"))
            .collect::<Vec<_>>()
            .join(", ");
        return PlanValidation::NeedsRevision {
            feedback: format!(
                "Plan overlaps with files being edited by other beads: {files_str}. \
                 Please avoid these files or wait for the other bead to complete."
            ),
        };
    }

    // Check for high-risk steps
    let high_risk_count = plan
        .steps
        .iter()
        .filter(|s| matches!(s.risk, PlanStepRisk::High))
        .count();

    if high_risk_count > plan.steps.len() / 2 {
        return PlanValidation::NeedsRevision {
            feedback: format!(
                "{high_risk_count} of {} steps are high-risk. \
                 Consider breaking this into smaller, safer changes.",
                plan.steps.len()
            ),
        };
    }

    PlanValidation::Approved
}

/// Generate a default plan from a handoff chain and bead metadata.
///
/// Used when `plan_gate` is enabled but the worker doesn't propose
/// a plan — the orchestrator generates one from available context.
pub fn generate_default_plan(files_changed: &[String], summary: &str) -> WorkerPlan {
    let steps: Vec<PlanStep> = files_changed
        .iter()
        .map(|f| PlanStep {
            description: format!("Modify {f}"),
            files: vec![f.clone()],
            risk: PlanStepRisk::Medium,
        })
        .collect();

    WorkerPlan {
        steps,
        estimated_files: files_changed.to_vec(),
        risk_assessment: format!("Auto-generated plan for: {summary}"),
    }
}
