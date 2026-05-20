//! Prompt assembly for dispatched agents.
//!
//! `build_prompt()` constructs the user-facing task prompt with bead context.
//! `build_system_prompt()` layers base instructions, golden rules, and agent definitions.

use std::path::Path;

use crate::bead::Bead;

/// Task framing varies by agent perspective so dispatched agents receive
/// role-appropriate instructions rather than a generic "fix this" prompt.
fn task_framing(agent_name: Option<&str>) -> &'static str {
    match agent_name.unwrap_or("dev-agent") {
        "staging-agent" => "Review this change. Verify tests validate real behavior, not mocks.",
        "prod-agent" => {
            "Audit this code for production readiness: resource leaks, error handling, concurrency."
        }
        "feature-agent" => {
            "Check cross-file coherence: dependencies, API contracts, error consistency."
        }
        "architect-agent" => {
            "Analyze this problem. Evaluate approaches, write an ADR, decompose into beads."
        }
        "pm-agent" => {
            "Assess from a strategic perspective: scope, cross-repo overlap, prioritization."
        }
        _ => "Fix this issue. Make the minimal change needed.",
    }
}

/// Build the prompt for a bead.
///
/// Includes the bead ID and repo path so the agent can self-manage its
/// lifecycle via MCP tools (comment, close). When a workspace path is
/// provided, reads the handoff chain for context from previous phases.
///
/// The prompt uses XML tags to separate sections so the model can
/// unambiguously parse task, context, and instructions.
pub fn build_prompt(
    bead: &Bead,
    repo_path: &str,
    workspace: Option<&Path>,
    agent_name: Option<&str>,
) -> String {
    let handoff_context = workspace
        .map(|ws| {
            let chain = crate::handoff::Handoff::read_chain(ws);
            crate::handoff::Handoff::format_for_prompt(&chain)
        })
        .unwrap_or_default();

    // Use workspace path for Repo: line (agent's actual working directory)
    // to prevent agents from resolving absolute paths against the main repo.
    // Keep repo_path for MCP bead tools where .beads/ lives.
    let work_repo = workspace
        .map(|ws| ws.display().to_string())
        .unwrap_or_else(|| repo_path.to_string());

    let framing = task_framing(agent_name);

    let handoff_section = if handoff_context.is_empty() {
        String::new()
    } else {
        format!("\n<handoff>\n{handoff_context}</handoff>\n")
    };

    format!(
        "<task>\n\
         {framing}\n\
         </task>\n\
         \n\
         <bead>\n\
         Bead ID: {bead_id}\n\
         Repo: {work_repo}\n\
         Title: {title}\n\
         Description: {desc}\n\
         </bead>\n\
         {handoff}\
         \n\
         <instructions>\n\
         After completing your work:\n\
         1. Run tests via `task test`\n\
         2. Commit: git commit -m \"[{bead_id}] type(scope): description\" (the [{bead_id}] prefix is REQUIRED)\n\
         3. Comment your status via mcp__rsry__rsry_bead_comment with repo_path=\"{bead_repo}\" and id=\"{bead_id}\"\n\
         4. Report what you changed\n\
         Do NOT close the bead yourself — the reconciler verifies and closes it.\n\
         </instructions>",
        bead_id = bead.id,
        bead_repo = repo_path,
        title = bead.title,
        desc = bead.description,
        handoff = handoff_section,
    )
}

/// Prompt version for traceability — agents include this in bead comments
/// so output can be traced back to the prompt configuration that produced it.
pub const PROMPT_VERSION: &str = "v0.2.0";

/// System prompt prepended to all dispatched agents.
/// Tells agents about available MCP tools, workflow expectations,
/// and bead lifecycle management.
const AGENT_SYSTEM_PROMPT: &str = "\
You are a rosary-dispatched agent working on a bead (work item).\n\
\n\
## Available Tools\n\
- **mache MCP** (`mcp__mache__*`): Structural code navigation — \
  find_definition, find_callers, find_callees, search, get_overview. \
  Prefer mache over grep for understanding code structure.\n\
- **rsry MCP** (`mcp__rsry__*`): Bead management — \
  bead_comment, bead_search, bead_link. You can comment and search but NOT close beads.\n\
\n\
## Workflow\n\
- Use `task build` / `task test` — never raw `cargo` or `go` commands. \
  The Taskfile runs linters and sets required env vars that raw commands skip.\n\
- Read the relevant code before making claims about it. \
  If you haven't opened a file, don't assert what it contains.\n\
- Make minimal, focused changes.\n\
- Commit format: `[BEAD-ID] type(scope): description` — the [BEAD-ID] prefix is mandatory.\n\
- Do NOT add co-author lines to commits.\n\
\n\
## Bead Lifecycle\n\
Your prompt includes a Bead ID and Repo path. Manage the bead throughout:\n\
1. **Comment progress** via `mcp__rsry__rsry_bead_comment` as you work — \
   not just at the end. Other agents and humans read these for context.\n\
2. Do NOT close the bead — the reconciler verifies your work and closes it.\n\
3. If you cannot fix the issue, comment explaining what you tried and why.\n\
";

/// Strip YAML frontmatter from a markdown file.
/// Frontmatter is delimited by `---` on its own line at the start.
pub fn strip_frontmatter(content: &str) -> String {
    if !content.starts_with("---") {
        return content.to_string();
    }
    // Find the closing "---" after the opening one
    if let Some(end) = content[3..].find("\n---") {
        let after = 3 + end + 4; // 3 for "---", end for body, 4 for "\n---"
        content[after..].trim_start_matches('\n').to_string()
    } else {
        content.to_string()
    }
}

/// Load an agent definition from its markdown file.
///
/// Reads `{agents_dir}/{agent_name}.md`, strips YAML frontmatter,
/// and returns the markdown body.
pub fn load_agent_prompt(agents_dir: &Path, agent_name: &str) -> Option<String> {
    let file_name = if agent_name.ends_with(".md") {
        agent_name.to_string()
    } else {
        format!("{agent_name}.md")
    };
    let path = agents_dir.join(&file_name);
    let content = std::fs::read_to_string(&path).ok()?;
    Some(strip_frontmatter(&content))
}

/// Load GOLDEN_RULES.md from the agents/rules/ directory.
fn load_golden_rules(agents_dir: &Path) -> Option<String> {
    let path = agents_dir.join("rules").join("GOLDEN_RULES.md");
    std::fs::read_to_string(&path).ok()
}

/// Build the complete system prompt for an agent dispatch.
///
/// Layers:
/// 1. Base AGENT_SYSTEM_PROMPT (MCP tools, workflow, bead lifecycle)
/// 2. GOLDEN_RULES.md (if agents_dir provided)
/// 3. Agent-specific definition (if agent_name set and file exists)
///
/// Falls back gracefully — missing files produce warnings, not errors.
pub fn build_system_prompt(agent_name: Option<&str>, agents_dir: Option<&Path>) -> String {
    let mut parts = vec![format!(
        "Prompt version: {PROMPT_VERSION}\n\n{AGENT_SYSTEM_PROMPT}"
    )];

    if let Some(dir) = agents_dir {
        if let Some(rules) = load_golden_rules(dir) {
            parts.push(format!("\n## Golden Rules\n\n{rules}"));
        } else {
            eprintln!(
                "[dispatch] warning: GOLDEN_RULES.md not found in {}",
                dir.display()
            );
        }

        if let Some(name) = agent_name {
            if let Some(agent_prompt) = load_agent_prompt(dir, name) {
                parts.push(format!("\n## Agent Perspective\n\n{agent_prompt}"));
                eprintln!("[dispatch] loaded agent definition: {name}");
            } else {
                eprintln!("[dispatch] warning: agent definition not found: {name}");
            }
        }
    }

    parts.join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bead::Bead;

    fn stub_bead(id: &str, title: &str) -> Bead {
        Bead {
            id: id.to_string(),
            title: title.to_string(),
            description: "Fix the thing.".to_string(),
            repo: "rosary".to_string(),
            status: "open".to_string(),
            issue_type: "bug".to_string(),
            priority: 1,
            owner: None,
            created_at: chrono::Utc::now(),
            updated_at: chrono::Utc::now(),
            dependency_count: 0,
            dependent_count: 0,
            comment_count: 0,
            branch: None,
            pr_url: None,
            jj_change_id: None,
            external_ref: None,
            files: vec![],
            test_files: vec![],
            created_by: None,
            scope: String::new(),
            derived_from: vec![],
        }
    }

    // --- task_framing ---

    #[test]
    fn task_framing_dev_agent_default() {
        assert!(task_framing(None).contains("Fix this issue"));
        assert!(task_framing(Some("dev-agent")).contains("Fix this issue"));
    }

    #[test]
    fn task_framing_known_agents() {
        assert!(task_framing(Some("staging-agent")).contains("Review"));
        assert!(task_framing(Some("prod-agent")).contains("production readiness"));
        assert!(task_framing(Some("feature-agent")).contains("coherence"));
        assert!(task_framing(Some("architect-agent")).contains("ADR"));
        assert!(task_framing(Some("pm-agent")).contains("strategic"));
    }

    #[test]
    fn task_framing_unknown_falls_back_to_default() {
        assert!(task_framing(Some("mystery-agent")).contains("Fix this issue"));
    }

    // --- strip_frontmatter ---

    #[test]
    fn strip_frontmatter_removes_yaml_block() {
        let input = "---\nstatus: draft\nauthor: james\n---\n\n# Heading\n\nBody text.";
        let out = strip_frontmatter(input);
        assert!(!out.contains("status:"));
        assert!(out.contains("# Heading"));
        assert!(out.contains("Body text."));
    }

    #[test]
    fn strip_frontmatter_no_frontmatter_passthrough() {
        let input = "# Just a heading\n\nSome text.";
        assert_eq!(strip_frontmatter(input), input);
    }

    #[test]
    fn strip_frontmatter_unclosed_returns_original() {
        // No closing `---` — return content unchanged rather than eating it all
        let input = "---\nstatus: draft\n\n# Heading";
        assert_eq!(strip_frontmatter(input), input);
    }

    // --- build_prompt ---

    #[test]
    fn build_prompt_contains_bead_id() {
        let bead = stub_bead("rosary-abc123", "Fix the thing");
        let prompt = build_prompt(&bead, "/repo/rosary", None, None);
        assert!(prompt.contains("rosary-abc123"));
    }

    #[test]
    fn build_prompt_contains_title_and_description() {
        let bead = stub_bead("rosary-abc123", "Fix the thing");
        let prompt = build_prompt(&bead, "/repo/rosary", None, None);
        assert!(prompt.contains("Fix the thing"));
        assert!(prompt.contains("Fix the thing."));
    }

    #[test]
    fn build_prompt_commit_format_instruction() {
        let bead = stub_bead("rosary-xyz", "A task");
        let prompt = build_prompt(&bead, "/repo/rosary", None, None);
        // Must include [bead-id] prefix convention
        assert!(
            prompt.contains("[rosary-xyz]"),
            "prompt must include bead-id commit prefix"
        );
    }

    #[test]
    fn build_prompt_no_handoff_section_without_workspace() {
        let bead = stub_bead("rosary-abc", "A task");
        let prompt = build_prompt(&bead, "/repo/rosary", None, None);
        assert!(
            !prompt.contains("<handoff>"),
            "no workspace = no handoff section"
        );
    }

    #[test]
    fn build_prompt_uses_workspace_path_for_repo_line() {
        let bead = stub_bead("rosary-abc", "A task");
        let ws = std::path::Path::new("/tmp/worktree/rosary-abc");
        let prompt = build_prompt(&bead, "/repo/rosary", Some(ws), None);
        // Repo: line should use workspace path, not repo_path
        assert!(prompt.contains("/tmp/worktree/rosary-abc"));
    }

    #[test]
    fn build_prompt_framing_varies_by_agent() {
        let bead = stub_bead("rosary-abc", "A task");
        let dev = build_prompt(&bead, "/r", None, Some("dev-agent"));
        let staging = build_prompt(&bead, "/r", None, Some("staging-agent"));
        assert!(dev.contains("Fix this issue"));
        assert!(staging.contains("Review"));
        assert_ne!(dev, staging);
    }

    // --- build_system_prompt ---

    #[test]
    fn build_system_prompt_includes_base_without_agents_dir() {
        let prompt = build_system_prompt(None, None);
        assert!(prompt.contains(PROMPT_VERSION));
        assert!(prompt.contains("rsry MCP"));
        assert!(prompt.contains("mache MCP"));
    }

    #[test]
    fn build_system_prompt_with_missing_agents_dir_still_returns_base() {
        let dir = std::path::Path::new("/nonexistent/agents");
        let prompt = build_system_prompt(Some("dev-agent"), Some(dir));
        assert!(prompt.contains(PROMPT_VERSION));
    }

    #[test]
    fn load_agent_prompt_returns_none_for_missing_file() {
        let dir = std::path::Path::new("/nonexistent");
        assert!(load_agent_prompt(dir, "ghost-agent").is_none());
    }

    #[test]
    fn load_agent_prompt_strips_frontmatter() {
        let tmp = tempfile::TempDir::new().unwrap();
        let content = "---\nowner: dev\n---\n\n# Dev Agent\n\nDo stuff.";
        std::fs::write(tmp.path().join("dev-agent.md"), content).unwrap();
        let loaded = load_agent_prompt(tmp.path(), "dev-agent").unwrap();
        assert!(!loaded.contains("owner:"));
        assert!(loaded.contains("# Dev Agent"));
    }

    #[test]
    fn load_agent_prompt_accepts_md_extension() {
        let tmp = tempfile::TempDir::new().unwrap();
        std::fs::write(tmp.path().join("test-agent.md"), "# Test").unwrap();
        assert!(load_agent_prompt(tmp.path(), "test-agent.md").is_some());
        assert!(load_agent_prompt(tmp.path(), "test-agent").is_some());
    }
}
