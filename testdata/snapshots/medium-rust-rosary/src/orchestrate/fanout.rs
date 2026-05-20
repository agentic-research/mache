//! Parallel research fan-out for the scoping phase.
#![allow(dead_code)]
//!
//! Instead of a single scoping-agent doing all research sequentially,
//! fan out multiple read-only workers in parallel to explore different
//! aspects of the codebase simultaneously.
//!
//! Modeled after Claude Code's batch skill pattern:
//! 1. Decompose research into independent queries
//! 2. Spawn workers with `ReadOnly` permission in parallel
//! 3. Collect findings
//! 4. Synthesize into implementation prompt

use serde::{Deserialize, Serialize};
use std::time::Duration;

/// Configuration for a parallel research fan-out.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FanOutConfig {
    /// Research queries to execute in parallel.
    pub queries: Vec<ResearchQuery>,
    /// Maximum number of concurrent workers.
    #[serde(default = "default_max_parallel")]
    pub max_parallel: usize,
    /// Timeout per worker.
    #[serde(default = "default_timeout_secs", with = "duration_secs")]
    pub timeout: Duration,
}

fn default_max_parallel() -> usize {
    3
}

fn default_timeout_secs() -> Duration {
    Duration::from_secs(300) // 5 minutes
}

mod duration_secs {
    use serde::{Deserialize, Deserializer, Serialize, Serializer};
    use std::time::Duration;

    pub fn serialize<S: Serializer>(d: &Duration, s: S) -> Result<S::Ok, S::Error> {
        d.as_secs().serialize(s)
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Duration, D::Error> {
        let secs = u64::deserialize(d)?;
        Ok(Duration::from_secs(secs))
    }
}

/// A single research query to be executed by a worker.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResearchQuery {
    /// Human-readable description of what to research.
    pub prompt: String,
    /// File paths to focus on (optional scope narrowing).
    #[serde(default)]
    pub scope: Vec<String>,
    /// The aspect of the problem this query explores.
    pub aspect: ResearchAspect,
}

/// What aspect of the problem a research query explores.
///
/// Used by the synthesis step to organize findings by concern.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResearchAspect {
    /// Understanding the current implementation.
    Implementation,
    /// Analyzing test coverage and quality.
    TestCoverage,
    /// Mapping dependencies and call graphs.
    Dependencies,
    /// Checking for similar patterns elsewhere in the codebase.
    Patterns,
    /// Reviewing documentation and comments.
    Documentation,
    /// Custom aspect.
    Custom(String),
}

/// Default research queries for a bead based on its issue type.
///
/// These are the "fan-out" queries that replace a single scoping-agent:
/// instead of one agent doing everything, three workers explore in parallel.
pub fn default_queries_for(issue_type: &str, title: &str) -> Vec<ResearchQuery> {
    match issue_type {
        "bug" => vec![
            ResearchQuery {
                prompt: format!(
                    "Investigate the bug described as: '{title}'. \
                     Find the relevant source files, understand the current behavior, \
                     and identify the root cause. Report specific file paths and line numbers."
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::Implementation,
            },
            ResearchQuery {
                prompt: format!(
                    "Find all tests related to the functionality described as: '{title}'. \
                     Report test coverage gaps, especially around edge cases and error paths. \
                     Do not modify any files."
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::TestCoverage,
            },
            ResearchQuery {
                prompt: format!(
                    "Map the dependency graph around the code related to: '{title}'. \
                     Who calls this code? What does it depend on? Are there related \
                     patterns elsewhere in the codebase? Report file paths and function names."
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::Dependencies,
            },
        ],
        "feature" => vec![
            ResearchQuery {
                prompt: format!(
                    "Research the codebase to understand where the feature '{title}' \
                     should be implemented. Find similar patterns, relevant modules, \
                     and integration points. Report specific file paths."
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::Implementation,
            },
            ResearchQuery {
                prompt: format!(
                    "Analyze the test infrastructure related to '{title}'. \
                     What testing patterns does this codebase use? What test files \
                     will need to be created or modified? Report the testing conventions."
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::TestCoverage,
            },
            ResearchQuery {
                prompt: format!(
                    "Check for existing patterns similar to '{title}' in the codebase. \
                     Are there conventions or abstractions that should be reused? \
                     Any documentation about the relevant module architecture?"
                ),
                scope: Vec::new(),
                aspect: ResearchAspect::Patterns,
            },
        ],
        _ => vec![ResearchQuery {
            prompt: format!(
                "Research the codebase to understand the context for: '{title}'. \
                 Find relevant files, understand the current implementation, \
                 and report specific file paths and observations."
            ),
            scope: Vec::new(),
            aspect: ResearchAspect::Implementation,
        }],
    }
}
