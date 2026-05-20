//! MCP tool definitions — JSON schema for every `rsry_*` tool.

use serde_json::{Value, json};

pub(crate) fn tool_definitions() -> Value {
    json!({
        "tools": [
            {
                "name": "rsry_scan",
                "description": "Scan all configured repos for beads (work items). Returns a JSON array of beads with their status, priority, and metadata.",
                "inputSchema": {
                    "type": "object",
                    "properties": {},
                    "required": []
                }
            },
            {
                "name": "rsry_status",
                "description": "Return aggregated status counts across all repos: open, ready, in_progress, and blocked bead counts.",
                "inputSchema": {
                    "type": "object",
                    "properties": {},
                    "required": []
                }
            },
            {
                "name": "rsry_list_beads",
                "description": "List beads with optional filters. Paginated to avoid oversized responses. Returns beads array + total count.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "status": {
                            "type": "string",
                            "description": "Filter by status (open, in_progress, blocked, ready, done, etc.). If omitted, returns all beads."
                        },
                        "repo": {
                            "type": "string",
                            "description": "Filter by repo name (e.g. 'rosary', 'mache'). If omitted, returns beads from all repos."
                        },
                        "limit": {
                            "type": "integer",
                            "description": "Max beads to return (default 50, max 200).",
                            "default": 50
                        },
                        "offset": {
                            "type": "integer",
                            "description": "Skip this many beads before returning results (for pagination).",
                            "default": 0
                        }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_run_once",
                "description": "Run a reconciliation pass. With bead_id: starts the full pipeline in the background (async — returns immediately with status 'started', use rsry_active to poll). Without bead_id: single synchronous pass across all beads. Use dry_run=true to preview without dispatching.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": {
                            "type": "string",
                            "description": "Target a specific bead. Starts pipeline in background (async). Use rsry_active to monitor. With dry_run=true, runs a single synchronous pass instead."
                        },
                        "dry_run": {
                            "type": "boolean",
                            "description": "If true, print what would be dispatched without actually spawning agents. Defaults to false — omitting this field will actually dispatch.",
                            "default": false
                        }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_bead_create",
                "description": "Create a new bead (work item) in a repo's Dolt database. Use when you've identified a discrete, actionable issue. Set file scopes accurately — they determine parallel dispatch safety via has_file_overlap(). Pass either `scope: 'repo:<name>'` (canonical) or `repo_path: '/path/to/repo'` (legacy).",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>' (bare names like 'rosary' also parse). Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "title": { "type": "string", "description": "Bead title" },
                        "description": { "type": "string", "description": "Bead description", "default": "" },
                        "priority": { "type": "integer", "description": "Priority 0-3 (0=P0 highest)", "default": 2 },
                        "issue_type": { "type": "string", "description": "Issue type: bug, feature, task, chore, review, epic, design, research", "default": "task" },
                        "owner": { "type": "string", "description": "Agent owner (dev-agent, staging-agent, etc.). Auto-assigned from issue_type if omitted." },
                        "files": { "type": "array", "items": { "type": "string" }, "description": "Source files this bead touches. CRITICAL: these scope parallel dispatch — has_file_overlap() (epic.rs:386-393) blocks concurrent beads sharing files, and reconcile.rs:372-380 enforces it at dispatch time. Set scopes ONLY after reading the code; guessed scopes cause false-negative overlap and agent collisions. Include both files being modified AND files needing wiring changes (imports, call sites). New files are safe — no overlap possible." },
                        "test_files": { "type": "array", "items": { "type": "string" }, "description": "Test files to validate the change. Also checked for overlap — two beads sharing a test file will be serialized, not parallelized." },
                        "depends_on": { "type": "array", "items": { "type": "string" }, "description": "Bead IDs this bead depends on (blocked until they complete). Creates entries in the dependencies table." }
                    },
                    "required": ["title"],
                    "anyOf": [
                        { "required": ["scope"] },
                        { "required": ["repo_path"] }
                    ]
                }
            },
            {
                "name": "rsry_bead_update",
                "description": "Update a bead's fields (PATCH semantics). Only provided fields are changed; omitted fields are left unchanged. Pass either `scope: 'repo:<name>'` (canonical) or `repo_path` (legacy).",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>' (bare names parse). Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "id": { "type": "string", "description": "Bead ID to update" },
                        "title": { "type": "string", "description": "New title" },
                        "description": { "type": "string", "description": "New description" },
                        "priority": { "type": "integer", "description": "New priority 0-3" },
                        "issue_type": { "type": "string", "description": "New issue type" },
                        "owner": { "type": "string", "description": "New owner/assignee" },
                        "files": { "type": "array", "items": { "type": "string" }, "description": "Updated source files list. These scope parallel dispatch — see has_file_overlap() (epic.rs:386-393). Verify against actual code before setting; inaccurate scopes cause agent collisions or missed overlap detection." },
                        "test_files": { "type": "array", "items": { "type": "string" }, "description": "Updated test files list. Also checked for overlap at dispatch time (reconcile.rs:372-380)." }
                    },
                    "required": ["id"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_close",
                "description": "Close a bead by ID, marking it as done. Use after your changes are committed and tests pass. Do not close if the fix is incomplete or tests are failing — comment explaining the state instead. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "id": { "type": "string", "description": "Bead ID to close" }
                    },
                    "required": ["id"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_comment",
                "description": "Add a progress comment to a bead. Use throughout your work to log what you've tried, found, and what remains. Other agents in the pipeline and human reviewers read these comments for context. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "id": { "type": "string", "description": "Bead ID" },
                        "body": { "type": "string", "description": "Comment text" }
                    },
                    "required": ["id", "body"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_comment_list",
                "description": "List comments on a bead with their stable comment_ids. Soft-deleted comments are hidden by default; pass include_deleted=true to see them. Use this to find a comment_id before calling rsry_bead_comment_update or rsry_bead_comment_delete. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "id": { "type": "string", "description": "Bead ID" },
                        "include_deleted": { "type": "boolean", "description": "If true, include soft-deleted comments", "default": false }
                    },
                    "required": ["id"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_comment_update",
                "description": "Edit the body of an existing comment. Records edit_reason in the audit trail and captures the prior body in original_text on the FIRST edit (immutable thereafter — subsequent edits do not rewrite original_text). Returns the updated comment. Use rsry_bead_comment_list to find comment_ids. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "comment_id": { "type": "string", "description": "Stable comment id (from rsry_bead_comment_list). Opaque string — Dolt produces UUIDs, SQLite produces stringified integers." },
                        "body": { "type": "string", "description": "New comment text" },
                        "reason": { "type": "string", "description": "Optional reason for the edit (recorded in edit_reason)" }
                    },
                    "required": ["comment_id", "body"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_comment_delete",
                "description": "Soft-delete a comment by setting deleted_at and optionally delete_reason. Audit trail (original_text, edit_reason, delete_reason) is preserved. Hard-delete is CLI-only — `rsry bead comment delete --hard` — never exposed via MCP. Idempotent: re-deleting refreshes both timestamp and reason without erroring. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "comment_id": { "type": "string", "description": "Stable comment id (from rsry_bead_comment_list). Opaque string — Dolt produces UUIDs, SQLite produces stringified integers." },
                        "reason": { "type": "string", "description": "Optional reason for the deletion. Persisted in the dedicated `delete_reason` column (independent of `edit_reason`) so it is preserved even when the comment was previously edited." }
                    },
                    "required": ["comment_id"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_bead_link",
                "description": "Add or remove a dependency between beads. Use to express 'A depends on B' (A is blocked until B completes). Cross-repo example (scope-only): rsry_bead_link(scope='repo:cloister', id='cloister-963a5c', depends_on='signet-9605a3') — `depends_on`'s 'signet-' prefix auto-routes through LinkageStore. Same-repo example: rsry_bead_link(repo_path='/path/to/cloister', id='cloister-963a5c', depends_on='cloister-aaaaaa') — same-repo deps still need `repo_path` until name→path resolution lands. Scope forms: 'repo:<name>' (canonical), 'external:<uri>' (zen inbox, cloister bundles), 'global' (org-level beads). `cross_repo` target is repo-only; reserved namespaces (global, external:) are rejected. At least one of `scope` or `repo_path` is required; passing both is allowed when they name the same repo (the handler rejects disagreement).",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>' | 'external:<uri>' | 'global'. Bare names like 'rosary' also parse as Repo. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory. Required for same-repo deps; for cross-scope (External/Global) use `scope` instead." },
                        "id": { "type": "string", "description": "Bead ID that depends on another (must live in the resolved scope)" },
                        "depends_on": { "type": "string", "description": "Bead ID of the prerequisite. Same-repo by default; if '<repo>-<id>' prefix names a different repo, the dep auto-routes via LinkageStore." },
                        "cross_repo": { "type": "string", "description": "Explicit cross-repo target as '<repo>/<bead-id>'. Overrides auto-detection. Repo-only — reserved namespaces (global, external:) are rejected." },
                        "remove": { "type": "boolean", "description": "If true, removes the dependency instead of adding", "default": false }
                    },
                    "required": ["id", "depends_on"],
                    "anyOf": [
                        { "required": ["scope"] },
                        { "required": ["repo_path"] }
                    ]
                }
            },
            {
                "name": "rsry_bead_search",
                "description": "Search beads in a specific repo by title/description substring. Returns matching beads with their status and metadata. Use to check for existing beads before creating duplicates. Pass either `scope` or `repo_path`.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "scope": { "type": "string", "description": "Canonical scope: 'repo:<name>'. Takes priority over repo_path." },
                        "repo_path": { "type": "string", "description": "Legacy: path to repo with .beads/ directory" },
                        "query": { "type": "string", "description": "Search query" },
                        "limit": { "type": "integer", "description": "Max results to return (default 20, max 50)", "default": 20, "minimum": 1, "maximum": 50 }
                    },
                    "required": ["query"],
                    "anyOf": [{ "required": ["scope"] }, { "required": ["repo_path"] }]
                }
            },
            {
                "name": "rsry_dispatch",
                "description": "Dispatch an agent to work on a specific bead. Spawns a Claude/Gemini agent in the bead's repo with the appropriate agent perspective (dev-agent, staging-agent, etc.) and permissions.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Bead ID to dispatch" },
                        "repo_path": { "type": "string", "description": "Path to repo containing the bead" },
                        "provider": { "type": "string", "description": "Agent provider (claude, gemini, acp)", "default": "claude" },
                        "agent": { "type": "string", "description": "Agent persona override (dev-agent, staging-agent, prod-agent, feature-agent, pm-agent). If omitted, uses bead owner." },
                        "isolate": { "type": "boolean", "description": "Create an isolated workspace (git worktree / jj workspace) before dispatch. Defaults to true. Set to false only for single-concurrency in-place execution.", "default": true }
                    },
                    "required": ["bead_id", "repo_path"]
                }
            },
            {
                "name": "rsry_active",
                "description": "Show currently running agent sessions with bead ID, repo, provider, elapsed time.",
                "inputSchema": {
                    "type": "object",
                    "properties": {},
                    "required": []
                }
            },
            {
                "name": "rsry_workspace_create",
                "description": "Create an isolated workspace (jj or git worktree) for a bead. Returns the workspace work_dir and vcs type. The conductor should call this before dispatching an agent.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Bead ID for the workspace" },
                        "repo_path": { "type": "string", "description": "Path to the repo root" }
                    },
                    "required": ["bead_id", "repo_path"]
                }
            },
            {
                "name": "rsry_workspace_checkpoint",
                "description": "Checkpoint a workspace: jj commit + bookmark. Returns the jj change ID. Call after agent completes, before cleanup.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Bead ID" },
                        "repo_path": { "type": "string", "description": "Path to the repo root" },
                        "message": { "type": "string", "description": "Commit message (default: agent work)" }
                    },
                    "required": ["bead_id", "repo_path"]
                }
            },
            {
                "name": "rsry_workspace_cleanup",
                "description": "Clean up a workspace (jj workspace forget + delete directory). Call after checkpoint.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Bead ID" },
                        "repo_path": { "type": "string", "description": "Path to the repo root" }
                    },
                    "required": ["bead_id", "repo_path"]
                }
            },
            {
                "name": "rsry_workspace_merge",
                "description": "Rebase the agent worktree branch onto base_branch, then push and open a PR when a remote is configured; in repos without a remote (e.g., local/test), perform a local fast-forward merge into base_branch instead.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Bead ID (branch is fix/{bead_id})" },
                        "repo_path": { "type": "string", "description": "Path to the repo root" },
                        "issue_type": { "type": "string", "description": "Issue type: bug, feature, task, chore, review, epic, design, research", "default": "task" },
                        "base_branch": { "type": "string", "description": "Branch to PR into. Defaults to \"main\" if omitted." }
                    },
                    "required": ["bead_id", "repo_path"]
                }
            },
            {
                "name": "rsry_decompose",
                "description": "Decompose a markdown document (ADR, README, etc.) into a decade of threaded beads. With commit=false (default) returns a preview — no beads created. With commit=true and repo_path set, creates beads in the repo, assigns them to threads in the backend lattice, deduplicates against existing beads, and enriches descriptions with success criteria and cross-references.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "path": { "type": "string", "description": "Absolute path to the markdown file" },
                        "title": { "type": "string", "description": "Title for the decade (defaults to first heading)" },
                        "model": { "type": "string", "description": "LLM model for gap-filling non-ADR docs (e.g. 'haiku', 'sonnet'). When set, runs claude -p to extract atoms from freeform markdown. Omit for ADR-shaped docs — structured parser handles those without LLM." },
                        "repo_path": { "type": "string", "description": "Absolute path to the repo root (.beads/ directory). Required when commit=true." },
                        "commit": { "type": "boolean", "description": "When true, create beads in repo_path. When false (default), returns preview only.", "default": false }
                    },
                    "required": ["path"]
                }
            },
            {
                "name": "rsry_pipeline_upsert",
                "description": "Write pipeline state for a bead to the backend store. Creates or updates the pipeline record tracking which agent phase a bead is in.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "repo": { "type": "string", "description": "Repository name (e.g. 'rosary')" },
                        "bead_id": { "type": "string", "description": "Bead ID (e.g. 'rsry-abc123')" },
                        "pipeline_phase": { "type": "integer", "description": "Phase index: dev=0, staging=1, prod=2, feature=3" },
                        "pipeline_agent": { "type": "string", "description": "Agent name (e.g. 'dev-agent')" },
                        "phase_status": { "type": "string", "description": "Sub-state: pending, executing, completed, failed", "default": "pending" },
                        "retries": { "type": "integer", "description": "Retry count", "default": 0 },
                        "consecutive_reverts": { "type": "integer", "description": "Consecutive revert count", "default": 0 },
                        "highest_verify_tier": { "type": "integer", "description": "Highest verification tier reached (optional)" },
                        "last_generation": { "type": "integer", "description": "Content hash generation", "default": 0 },
                        "backoff_until": { "type": "string", "description": "ISO 8601 datetime for retry eligibility (optional)" }
                    },
                    "required": ["repo", "bead_id", "pipeline_phase", "pipeline_agent"]
                }
            },
            {
                "name": "rsry_pipeline_query",
                "description": "Query pipeline state. Get a single pipeline by repo + bead_id, or list all active pipelines with no args.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "repo": { "type": "string", "description": "Repository name" },
                        "bead_id": { "type": "string", "description": "Bead ID" }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_dispatch_record",
                "description": "Record a dispatch event in the backend store. Called by the conductor/orchestrator when spawning an agent — not typically called by agents directly.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "id": { "type": "string", "description": "Dispatch UUID" },
                        "repo": { "type": "string", "description": "Repo name" },
                        "bead_id": { "type": "string", "description": "Bead ID" },
                        "agent": { "type": "string", "description": "Agent name" },
                        "provider": { "type": "string", "description": "Provider (claude, gemini, acp)" },
                        "work_dir": { "type": "string", "description": "Working directory" }
                    },
                    "required": ["id", "repo", "bead_id", "agent", "provider", "work_dir"]
                }
            },
            {
                "name": "rsry_dispatch_history",
                "description": "Query dispatch history. Filter by bead_id to see all dispatches for a specific bead, or use active_only to see currently running agents. Useful for checking if an agent is already working on a bead before dispatching another.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "bead_id": { "type": "string", "description": "Filter by bead ID" },
                        "active_only": { "type": "boolean", "description": "Only active dispatches", "default": true }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_decade_list",
                "description": "List decades (ADR-level organizing primitives). Optionally filter by status (proposed, active, completed, superseded).",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "status": { "type": "string", "description": "Filter by status (optional)" }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_decade_create",
                "description": "Create a decade (ADR-level grouping). Idempotent: re-creating with the same title + source_path returns `action: \"existed\"`. Conflicts (same id, different title) error rather than silently overwrite. Returns the created/existing decade's metadata so callers can chain decade → thread → bead without an intermediate read step.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "id": { "type": "string", "description": "Decade slug (kebab-case, e.g. 'substrate-idl')" },
                        "title": { "type": "string", "description": "Human-readable decade title" },
                        "source_path": { "type": "string", "description": "Optional path to ADR / design doc that motivates the decade" },
                        "status": { "type": "string", "description": "Decade status (default 'active'). One of: proposed, active, completed, superseded.", "default": "active" }
                    },
                    "required": ["id", "title"]
                }
            },
            {
                "name": "rsry_thread_list",
                "description": "List threads within a decade, or find the thread a bead belongs to.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "decade_id": { "type": "string", "description": "Decade ID to list threads for" },
                        "bead_id": { "type": "string", "description": "Find thread for this bead (alternative to decade_id)" },
                        "repo": { "type": "string", "description": "Repo name for bead lookup (required with bead_id)" }
                    },
                    "required": []
                }
            },
            {
                "name": "rsry_thread_create",
                "description": "Create a thread under a named decade. Refuses to land an orphan thread when the parent decade is missing (use rsry_decade_create first). Idempotent on identical payload. Returns the created/existing thread's metadata including the derived feature_branch.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "decade_id": { "type": "string", "description": "Parent decade ID (must already exist; create via rsry_decade_create)" },
                        "id": { "type": "string", "description": "Thread slug (conventionally '<decade-id>/<name>', e.g. 'substrate-idl/schema-bridge')" },
                        "name": { "type": "string", "description": "Human-readable thread title" }
                    },
                    "required": ["decade_id", "id", "name"]
                }
            },
            {
                "name": "rsry_thread_assign",
                "description": "Assign a bead to a thread. Creates the thread if it doesn't exist (auto-creates an 'ungrouped' decade if no parent given). For explicit decade/thread setup with metadata returns, prefer rsry_decade_create + rsry_thread_create first.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "thread_id": { "type": "string", "description": "Thread ID (e.g. 'ADR-003/pipeline-quality')" },
                        "thread_name": { "type": "string", "description": "Thread display name (for new threads)" },
                        "decade_id": { "type": "string", "description": "Decade this thread belongs to (for new threads)" },
                        "bead_id": { "type": "string", "description": "Bead ID to assign to the thread" },
                        "repo": { "type": "string", "description": "Repo name for the bead" }
                    },
                    "required": ["thread_id", "bead_id", "repo"]
                }
            },
            {
                "name": "rsry_thread_reparent",
                "description": "Move an existing thread under a different decade. Useful when threads land in 'ungrouped' or 'auto-discovered' and should belong to a real decade. Auto-creates the target decade if missing.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "thread_id": { "type": "string", "description": "Thread ID to re-parent (e.g. 'agentic-provenance/agent-identity')" },
                        "decade_id": { "type": "string", "description": "New decade ID (e.g. 'agentic-provenance')" },
                        "name": { "type": "string", "description": "Optional new thread name (keeps existing if omitted)" }
                    },
                    "required": ["thread_id", "decade_id"]
                }
            },
            {
                "name": "rsry_repo_register",
                "description": "Register a repo for the current user. Stores the repo URL so rsry can clone and dispatch agents to it. Requires backend store.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "repo_url": { "type": "string", "description": "Git clone URL (https://github.com/org/repo.git)" },
                        "repo_name": { "type": "string", "description": "Short name for the repo (e.g. 'rosary'). Defaults to last path component." }
                    },
                    "required": ["repo_url"]
                }
            },
            {
                "name": "rsry_repo_list",
                "description": "List repos registered by the current user.",
                "inputSchema": {
                    "type": "object",
                    "properties": {},
                    "required": []
                }
            },
            {
                "name": "rsry_bead_import",
                "description": "Import beads from a JSON array. Routes each bead to the correct repo using its 'repo' field (matched against configured repo names). Falls back to repo_path if no per-bead repo. Skips duplicates by exact title match. Use with `rsry bead export` for cross-instance migration.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "repo_path": { "type": "string", "description": "Default repo path — used for beads without a 'repo' field. Optional when all beads have 'repo' set." },
                        "beads": {
                            "type": "array",
                            "description": "Array of bead objects to import",
                            "items": {
                                "type": "object",
                                "properties": {
                                    "repo": { "type": "string", "description": "Target repo name (e.g. 'rosary', 'mache'). Matched against configured repos. Falls back to repo_path param." },
                                    "title": { "type": "string" },
                                    "description": { "type": "string" },
                                    "priority": { "type": "integer" },
                                    "issue_type": { "type": "string" },
                                    "files": { "type": "array", "items": { "type": "string" } },
                                    "test_files": { "type": "array", "items": { "type": "string" } }
                                },
                                "required": ["title"]
                            }
                        }
                    },
                    "required": ["beads"]
                }
            },
            {
                "name": "rsry_ticket_load",
                "description": "Consolidate Linear ticket context (issue body, comments, linked GitHub URL, Zendesk URL, existing tracking bead) into one response. Phase 0 of the Linear-escalation-triage workflow (rosary-5d7141). Replaces 4-5 manual lookups per ticket. Requires LINEAR_API_KEY in env or [linear].api_key in ~/.rsry/config.toml.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "ticket_id": {
                            "type": "string",
                            "description": "Linear identifier (e.g. 'CUS-495') or full URL ('https://linear.app/team/issue/CUS-495')"
                        }
                    },
                    "required": ["ticket_id"]
                }
            }
        ]
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tool_definitions_has_expected_tools() {
        let defs = tool_definitions();
        let tools = defs["tools"].as_array().unwrap();
        let names: Vec<&str> = tools.iter().map(|t| t["name"].as_str().unwrap()).collect();

        // Verify all tools have rsry_ prefix (no typos or missing names)
        for name in &names {
            assert!(
                name.starts_with("rsry_"),
                "tool '{name}' missing rsry_ prefix"
            );
        }

        // Verify critical tools exist (not exhaustive — adding a tool shouldn't break this test)
        for required in [
            "rsry_scan",
            "rsry_bead_create",
            "rsry_bead_close",
            "rsry_dispatch",
            "rsry_active",
            "rsry_workspace_create",
        ] {
            assert!(
                names.contains(&required),
                "required tool '{required}' missing"
            );
        }

        // Sanity: at least 15 tools (grows over time, never shrinks)
        assert!(
            tools.len() >= 15,
            "expected at least 15 tools, got {}",
            tools.len()
        );
    }

    #[test]
    fn tool_definitions_have_input_schemas() {
        let defs = tool_definitions();
        let tools = defs["tools"].as_array().unwrap();
        for tool in tools {
            assert!(
                tool.get("inputSchema").is_some(),
                "tool {} missing inputSchema",
                tool["name"]
            );
            assert_eq!(tool["inputSchema"]["type"], "object");
        }
    }

    #[test]
    fn thread_reparent_tool_present() {
        let defs = tool_definitions();
        let tools = defs["tools"].as_array().unwrap();
        let names: Vec<&str> = tools.iter().filter_map(|t| t["name"].as_str()).collect();
        assert!(
            names.contains(&"rsry_thread_reparent"),
            "rsry_thread_reparent must be in tool definitions"
        );
        let reparent = tools
            .iter()
            .find(|t| t["name"] == "rsry_thread_reparent")
            .unwrap();
        let required = reparent["inputSchema"]["required"].as_array().unwrap();
        let required_names: Vec<&str> = required.iter().filter_map(|v| v.as_str()).collect();
        assert!(required_names.contains(&"thread_id"));
        assert!(required_names.contains(&"decade_id"));
    }

    /// rosary-5dc9b0: `rsry_ticket_load` is registered + lists `ticket_id` as
    /// required. Pins the MCP contract for the Linear-escalation-triage tool.
    #[test]
    fn ticket_load_tool_is_registered() {
        let defs = tool_definitions();
        let tools = defs["tools"].as_array().unwrap();
        let ticket_load = tools
            .iter()
            .find(|t| t["name"] == "rsry_ticket_load")
            .expect("rsry_ticket_load must be in tool definitions");
        let required = ticket_load["inputSchema"]["required"].as_array().unwrap();
        let required_names: Vec<&str> = required.iter().filter_map(|v| v.as_str()).collect();
        assert!(
            required_names.contains(&"ticket_id"),
            "rsry_ticket_load must require ticket_id; got: {required_names:?}"
        );
    }

    #[test]
    fn bead_crud_tools_have_repo_path() {
        let defs = tool_definitions();
        let tools = defs["tools"].as_array().unwrap();
        for tool in tools {
            let name = tool["name"].as_str().unwrap();
            if name.starts_with("rsry_bead_") {
                let props = &tool["inputSchema"]["properties"];
                assert!(
                    props.get("repo_path").is_some(),
                    "{name} missing repo_path parameter"
                );
            }
        }
    }
}
