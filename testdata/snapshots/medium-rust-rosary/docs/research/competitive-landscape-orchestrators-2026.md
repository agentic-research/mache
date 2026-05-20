# Competitive Landscape: Autonomous Agent Orchestrators (March 2026)

This document surveys the major autonomous AI coding agent orchestrators and compares them
against rosary's architecture. Each system is evaluated on: dispatch model, verification
model, multi-repo support, trust/review model, cost model, and differentiation.

______________________________________________________________________

## 1. Gas Town / Wasteland (Steve Yegge)

**Website:** [gastownhall.ai](https://gastownhall.ai/) |
**Source:** [github.com/steveyegge/gastown](https://github.com/steveyegge/gastown) |
**Language:** Go |
**Released:** January 1, 2026

### Dispatch Model

- The Mayor (a Claude Code instance) orchestrates multiple agents called Polecats
- `gt sling <bead-id> <rig>` assigns work units (beads) to agents across rigs
- Agents work in persistent git worktrees (called "hooks") that survive crashes
- Convoys bundle multiple work units for coordinated dispatch
- Cross-rig dispatch via worktrees preserves agent identity attribution

### Verification Model

- Human-in-the-loop: `--notify --human` flags on convoys for oversight
- Problems view (`gt feed --problems`) detects stuck agents
- No automated tiered verification pipeline documented
- Relies on Git PR review as the final quality gate

### Multi-Repo Support

- Rigs encapsulate projects (one rig per git repo)
- `gt rig add <name> <repo-url>` to register repos
- Convoys coordinate work across multiple rigs within a workspace
- Cross-rig sling dispatches work to other rigs' agents

### Trust / Review Model

- **The Wasteland** is a federated trust network linking Gas Towns
- Three actors: rigs, posters, validators
- Trust levels: registered (L1) -> contributor (L2) -> maintainer (L3)
- Multi-dimensional stamps: quality, reliability, creativity scores with confidence level
- Git fork/merge model as the trust backbone
- Stamps accumulate into portable professional reputation

### Cost Model

- Open source, model-agnostic
- Costs depend on configured runtime provider (Claude, Codex, Cursor, etc.)
- No subscription; BYOK (bring your own key)

### What Rosary Has That Gas Town Doesn't

- **5-tier automated verification pipeline** (commit/compile/test/lint/diff-sanity)
- **Dolt-backed bead persistence** with SQL queryability and version-controlled history
- **Pipeline phase advancement** with structured handoffs (dev -> staging -> prod)
- **Exponential backoff + deadletter** for failed dispatches (5 retries, 30min max)
- **Cross-repo dependency tracking** with semantic dedup and file-overlap detection
- **Linear integration** for bidirectional human-facing issue tracking
- **BDR hierarchy** (decades/threads/beads) with formal decomposition pipeline

### What Gas Town Has That Rosary Needs

- **Federated trust network** (Wasteland) for multi-team/multi-org collaboration
- **Portable reputation stamps** with multi-dimensional attestation
- **Runtime-agnostic dispatch** to 9+ agent runtimes (Cursor, Codex, Amp, etc.)
- **TUI feed** (`gt feed`) for real-time agent activity monitoring
- **Convoy semantics** for bundling related work with human notification hooks
- **Community momentum** and extensive blog/documentation ecosystem

______________________________________________________________________

## 2. Sweep (AI Junior Dev)

**Website:** [sweep.dev](https://sweep.dev/) |
**Source:** [github.com/sweepai/sweep](https://github.com/sweepai/sweep) |
**Origin:** Y Combinator |
**Status:** Pivoted from GitHub bot to JetBrains AI coding assistant

### Dispatch Model

- Originally: GitHub issue triggers autonomous PR generation
- Current: JetBrains IDE plugin with agent, inline editing, and code review
- Single-agent model (no multi-agent orchestration)
- Reads codebase, plans modifications, generates PRs from natural language

### Verification Model

- 92% success rate on issue resolution in internal benchmarks
- PR-based review cycle — human reviews generated PR
- No automated multi-tier verification

### Multi-Repo Support

- GitHub integration scopes to individual repositories
- No cross-repo coordination or dependency tracking

### Trust / Review Model

- PRs require human merge approval
- No tiered trust or reputation system

### Cost Model

- Pivoted to JetBrains plugin with freemium model
- Discussion of potential service discontinuation (March 2026)
- Hosted and self-hosted deployment options

### What Rosary Has That Sweep Doesn't

- Multi-agent orchestration with parallel dispatch
- Cross-repo awareness and dependency tracking
- Automated verification pipeline
- Pipeline phases with handoffs between agent perspectives
- Stable Dolt-backed persistence (Sweep's future uncertain)

### What Sweep Has That Rosary Needs

- **Direct IDE integration** (JetBrains plugin)
- **Issue-to-PR automation** from GitHub issues (original model)
- Minimal: Sweep's pivot away from autonomous orchestration makes it less relevant

______________________________________________________________________

## 3. Devin (Cognition)

**Website:** [devin.ai](https://devin.ai/) |
**Source:** Proprietary |
**Pricing:** From $20/month (Devin 2.0)

### Dispatch Model

- Cloud-hosted autonomous agent in a sandboxed environment (shell + browser + editor)
- Interactive Planning: users collaborate with Devin on task scoping before execution
- Fleet mode: deploy parallel Devins across hundreds of repos for batch work
- Single-agent per session, but sessions can be parallelized externally

### Verification Model

- Devin Review: automated PR analysis categorized by severity (red/yellow/gray)
- Internal model self-verification with dynamic re-planning on roadblocks
- SWE-bench score: 13.86% (7x improvement over earlier versions)
- Devin 2.0 completes 83% more tasks per compute unit vs 1.x

### Multi-Repo Support

- Add any codebases for Devin to manage from a unified interface
- Deploy fleets of Devins across hundreds of repositories
- No cross-repo dependency graph or coordination between sessions

### Trust / Review Model

- Human reviews generated PRs
- Devin Review as an automated first-pass code review layer
- No tiered agent permissions or hierarchical trust model
- Enterprise: Goldman Sachs pilot with 12,000 developers

### Cost Model

- Individual: $20/month minimum ($2.25 per Agent Compute Unit)
- Team: $500/month (250 credits included)
- Enterprise: custom pricing
- Hosted only (no self-hosted option)

### What Rosary Has That Devin Doesn't

- **Multi-agent pipeline** (dev -> staging -> prod -> feature perspectives on same work)
- **Structured verification pipeline** (5 tiers, language-aware)
- **Cross-repo dependency tracking** with thread sequencing
- **Work decomposition** (BDR: decades -> threads -> beads) vs flat task list
- **Open source + self-hosted** vs proprietary cloud-only
- **Model agnostic** (Claude, Gemini, Qwen) vs locked to Cognition's model stack
- **Dolt version-controlled state** with SQL queryability

### What Devin Has That Rosary Needs

- **Browser-equipped sandboxed environment** for documentation lookup, API exploration
- **Interactive planning** with human-in-the-loop task scoping
- **Fleet-scale parallel dispatch** across hundreds of repos simultaneously
- **Devin Search** for codebase Q&A (rosary delegates this to mache)
- **Polished UI/UX** with web dashboard for non-terminal users
- **Enterprise sales motion** and brand recognition

______________________________________________________________________

## 4. OpenHands (Open-Source Devin)

**Website:** [openhands.dev](https://openhands.dev/) |
**Source:** [github.com/OpenHands/OpenHands](https://github.com/OpenHands/OpenHands) |
**License:** MIT |
**Origin:** Formerly OpenDevin, All-Hands-AI

### Dispatch Model

- Hierarchical agent delegation: coding agent can delegate subtasks to browser agent
- Event-sourced state model with deterministic replay
- Docker-containerized sessions (V0) moving to optional sandboxing with LocalWorkspace (V1)
- V1 SDK: composable agents with typed tools and MCP integration

### Verification Model

- Event log provides deterministic replay for debugging and verification
- Docker sandbox isolates agent actions from host
- No structured multi-tier verification pipeline
- Model-agnostic: supports any LLM backend

### Multi-Repo Support

- Per-session repo mounting (one repo per Docker container)
- No cross-repo dependency tracking or coordination
- Cloud self-hosted option for enterprise deployment

### Trust / Review Model

- Full transparency: event-sourced actions are auditable
- Community-driven (188+ contributors)
- No tiered permission system for agents
- Human reviews PRs generated by agents

### Cost Model

- Open source (MIT): free self-hosted
- Cloud Individual: free with BYOK (bring your own key)
- Cloud Growth: $500/month (unlimited users, RBAC)
- Cloud Enterprise: custom (self-hosted VPC, SAML/SSO)

### What Rosary Has That OpenHands Doesn't

- **Cross-repo orchestration** with bead/thread/decade hierarchy
- **Multi-perspective pipeline** (dev/staging/prod agents review same work)
- **5-tier verification pipeline** with language-aware checks
- **Dolt-backed persistent state** with version-controlled history
- **Priority triage scoring** (40% priority, 30% dependency, 20% age, 10% retry penalty)
- **Thread sequencing** for ordered bead dispatch

### What OpenHands Has That Rosary Needs

- **Docker sandboxing** for untrusted code execution
- **Browser agent** for web-based research during development
- **Event-sourced state** with deterministic replay (better debugging than log files)
- **V1 SDK** for composable agent construction
- **MCP tool integration** as first-class SDK primitive
- **Cloud-hosted option** with team management and RBAC

______________________________________________________________________

## 5. SWE-agent (Princeton NLP Group)

**Website:** [swe-agent.com](https://swe-agent.com/) |
**Source:** [github.com/SWE-agent/SWE-agent](https://github.com/SWE-agent/SWE-agent) |
**License:** MIT |
**Published:** NeurIPS 2024

### Dispatch Model

- Single-agent: takes one GitHub issue, produces one patch
- Agent-Computer Interface (ACI): custom tool interface for file editing, navigation, testing
- Deploys in Docker containers (local or remote via Modal/AWS)
- Mini-SWE-Agent: 100-line version scoring 74% on SWE-bench Verified
- SWE-ReX: separate package managing deployment environments

### Verification Model

- Best-of-N sampling with verifier model scoring trajectories post-hoc
- Patch-level evaluation against ground-truth test suites (SWE-bench)
- No integrated CI/CD or multi-tier verification
- Academic focus: optimizing pass@K metrics

### Multi-Repo Support

- Single-repo, single-issue focused
- Multi-SWE-bench evaluates across 7 programming languages
- No cross-repo coordination

### Trust / Review Model

- No trust model — academic research tool
- Generated patches evaluated against test suites
- Human reviews output patches manually

### Cost Model

- Open source (MIT): free
- BYOK for LLM inference (GPT-4o, Claude Sonnet, etc.)
- Compute costs for Docker/Modal/AWS deployment

### What Rosary Has That SWE-agent Doesn't

- **Production orchestration** vs academic benchmark tool
- **Multi-agent pipeline** with phase advancement and handoffs
- **Cross-repo coordination** and dependency tracking
- **Persistent state** (Dolt) vs ephemeral per-run state
- **Priority triage** and workqueue management
- **Exponential backoff and deadletter** for reliability

### What SWE-agent Has That Rosary Needs

- **Agent-Computer Interface (ACI)** design — purpose-built tool interfaces for agents
- **Best-of-N sampling with verifier** — generate multiple solutions, pick best
- **Benchmark integration** (SWE-bench) for measuring agent capability
- **Minimal-agent design** (mini-SWE-agent) — proof that 100 lines can score 74%

______________________________________________________________________

## 6. Aider (Paul Gauthier)

**Website:** [aider.chat](https://aider.chat/) |
**Source:** [github.com/Aider-AI/aider](https://github.com/Aider-AI/aider) |
**License:** Apache 2.0

### Dispatch Model

- Interactive terminal-based pair programming (not autonomous orchestration)
- Single-agent, single-repo, human-in-the-loop
- Repo map: tree-sitter based function signature extraction for context
- Edit formats: whole-file, diff, udiff depending on model capability
- Tight git integration: auto-commits each edit with descriptive messages

### Verification Model

- Git commit per edit — easy rollback
- Pre-commit hooks optional (`--git-commit-verify`)
- Linting integration available
- No automated multi-tier verification

### Multi-Repo Support

- Single repo only
- `/read` command can include read-only files from another repo
- No cross-repo orchestration

### Trust / Review Model

- Human-in-the-loop at every step
- Git history provides full audit trail
- No autonomous dispatch or trust hierarchy

### Cost Model

- Open source (Apache 2.0): free
- BYOK for LLM inference
- Supports 130+ languages via tree-sitter

### What Rosary Has That Aider Doesn't

- **Autonomous dispatch** — rosary runs unattended, aider requires a human
- **Multi-agent orchestration** with parallel dispatch
- **Cross-repo coordination** and dependency tracking
- **Pipeline phases** (dev/staging/prod perspectives)
- **Automated verification pipeline**
- **Work decomposition** (decades/threads/beads)

### What Aider Has That Rosary Needs

- **Repo map** — tree-sitter based codebase understanding (rosary delegates to mache)
- **Edit format flexibility** — different edit strategies per model capability
- **Reasoning effort controls** (`/reasoning-effort`, `/think-tokens`)
- **Broad model support** — works with virtually any LLM including local models
- **Low barrier to entry** — `pip install aider-chat` and go

______________________________________________________________________

## 7. Cline

**Website:** [cline.bot](https://cline.bot/) |
**Source:** [github.com/cline/cline](https://github.com/cline/cline) |
**License:** Apache 2.0 |
**Users:** 5M+ developers

### Dispatch Model

- VS Code extension with Plan/Act modes
- Subagents: read-only parallel research agents for codebase exploration
- MCP integration for external tool access
- Terminal command execution, file editing, browser control (via Computer Use)
- Human approves every file change and terminal command

### Verification Model

- Human-in-the-loop: every action requires approval (configurable)
- Timestamped audit logs for all actions
- No automated verification pipeline
- Browser-based visual verification via screenshots

### Multi-Repo Support

- Works within VS Code workspace (can include multiple folders)
- No cross-repo orchestration or dependency tracking
- Subagents scoped to current workspace

### Trust / Review Model

- Per-action approval model: human sees and approves each change
- Traceable, timestamped logs for post-hoc auditability
- No tiered trust or agent hierarchy
- Enterprise version adds centralized governance

### Cost Model

- Extension: free and open source
- AI inference: BYOK (pay your provider directly, no markup)
- Teams: free through Q1 2026, then $20/month (first 10 seats free)
- Enterprise: custom pricing with centralized billing

### What Rosary Has That Cline Doesn't

- **Autonomous unattended dispatch** vs human-in-the-loop per action
- **Multi-perspective pipeline** (dev/staging/prod)
- **Cross-repo orchestration** with dependency tracking
- **Automated verification pipeline** with language-aware checks
- **Work decomposition** and priority triage
- **Persistent state** in Dolt vs ephemeral session state

### What Cline Has That Rosary Needs

- **IDE integration** (VS Code) for interactive work alongside autonomous dispatch
- **Browser control** via Computer Use for visual verification and web research
- **MCP-first tool integration** with broad ecosystem support
- **5M+ user base** — proven developer adoption path
- **Subagent pattern** for read-only parallel exploration

______________________________________________________________________

## 8. Roo Code

**Website:** [roocode.com](https://roocode.com/) |
**Source:** [github.com/RooCodeInc/Roo-Code](https://github.com/RooCodeInc/Roo-Code) |
**License:** Apache 2.0

### Dispatch Model

- VS Code extension with role-based modes: Architect, Code, Debug, Ask, Custom
- **Boomerang Tasks** (Orchestrator Mode): main agent dispatches sub-agents in parallel
- Modes are tool-scoped — each mode limits available tools to its role
- Smart mode switching: agents request handoff to appropriate mode
- Roomate collaboration for shared development environments

### Verification Model

- Role-based isolation: Debug mode for testing/verification
- Human approves actions (configurable per mode)
- No automated multi-tier verification pipeline
- Browser control for integration/E2E testing

### Multi-Repo Support

- VS Code workspace scoped (multi-folder workspaces supported)
- No cross-repo orchestration or dependency tracking
- Cloud tasks can be dispatched remotely

### Trust / Review Model

- Zero-trust-compatible: all execution local, no hidden telemetry
- Mode-based tool restrictions limit what each agent role can do
- Human approval required for file changes and commands
- Enterprise: centralized billing and team management

### Cost Model

- Extension: free and open source
- Cloud Free: token tracking, task sharing
- Cloud Pro: $20/month + $5/hour for cloud tasks
- Cloud Team: $99/month + $5/hour (unlimited members)
- Model inference: BYOK at-cost, no markup

### What Rosary Has That Roo Code Doesn't

- **Autonomous cross-repo orchestration** vs workspace-scoped IDE extension
- **Persistent work state** in Dolt vs ephemeral session state
- **Multi-perspective pipeline** (dev/staging/prod agents)
- **5-tier automated verification** with language-aware checks
- **Priority triage and workqueue** management
- **Thread sequencing** for ordered related work
- **Linear integration** for human-facing issue tracking

### What Roo Code Has That Rosary Needs

- **Role-based modes** with scoped tool access (Architect/Code/Debug/Ask)
- **Smart mode switching** — agents recognize when to hand off to a different role
- **IDE integration** for interactive alongside autonomous work
- **Cloud task dispatch** for remote execution
- **Roomate collaboration** for shared sessions
- **Broad model support** (GPT-5.x, Gemini, Claude, open-weight models)

______________________________________________________________________

## 9. Claude Code (Anthropic)

**Website:** [claude.com/product/claude-code](https://claude.com/product/claude-code) |
**Source:** [github.com/anthropics/claude-code](https://github.com/anthropics/claude-code) |
**Revenue:** ~$2B annualized (Jan 2026) |
**Impact:** ~4% of all public GitHub commits (135K/day, Feb 2026)

### Dispatch Model

- Terminal-native agentic loop: plan -> execute -> verify, repeating until complete
- Headless mode (`-p` flag) for CI/CD and programmatic dispatch
- Python + TypeScript SDKs with structured outputs and tool approval callbacks
- Subagent spawning for parallel subtasks
- Task system with dependency graph + async mailboxes for team coordination
- **Worktree isolation** (v2.1.49+) for parallel execution
- WorktreeCreate/WorktreeRemove hooks for custom integration
- 200K context (1M in beta)

### Verification Model

- Self-verification within the agentic loop (run tests, check output)
- No external multi-tier verification pipeline
- Pre-commit hooks and linting integration
- Relies on human PR review as final gate
- Auto-approve mode: users grant more autonomy over time (20% at start, 40%+ by 750 sessions)

### Multi-Repo Support

- `claude -p` with `xargs -P` for parallel multi-repo dispatch
- Each headless session is independent (no cross-repo coordination)
- Worktree hooks (v2.1.50) enable custom orchestration layers
- Budget caps per session

### Trust / Review Model

- Graduated autonomy: users control auto-approve percentage
- Permission governance via allowed tools configuration
- Skill loading for specialized capabilities
- No formal trust hierarchy between agent instances
- Human merges PRs as review gate

### Cost Model

- Pro: $20/month (includes Claude Code access with Sonnet 4.5 default)
- Max: higher usage limits
- Team/Enterprise: custom pricing
- API pricing for headless/SDK usage

### What Rosary Has That Claude Code Doesn't

- **Cross-repo orchestration** with dependency tracking and thread sequencing
- **Multi-perspective pipeline** (dev -> staging -> prod -> feature agents review same work)
- **5-tier automated verification** (compile/test/lint/diff-sanity)
- **Dolt-backed persistent state** with version-controlled bead history
- **Work decomposition** (BDR: decades -> threads -> beads)
- **Priority triage** with composite scoring
- **Exponential backoff + deadletter** for reliability
- **Model-agnostic dispatch** (Claude, Gemini, Qwen vs Anthropic-only)
- **Linear integration** for human-facing project management

### What Claude Code Has That Rosary Needs

- **Massive adoption** (4% of GitHub commits, $2B revenue)
- **SDK** (Python + TypeScript) for building on top of the agent
- **Worktree hooks** as official extension points (rosary uses these)
- **Context compression** for long sessions
- **Subagent spawning** with async mailboxes
- **Auto-approve graduated trust** based on session history
- **1M context window** (beta) for large codebases

______________________________________________________________________

## Comparison Matrix

| Dimension         | Rosary                             | Gas Town                 | Sweep                | Devin                | OpenHands               | SWE-agent            | Aider            | Cline                | Roo Code               | Claude Code             |
| ----------------- | ---------------------------------- | ------------------------ | -------------------- | -------------------- | ----------------------- | -------------------- | ---------------- | -------------------- | ---------------------- | ----------------------- |
| **Dispatch**      | Multi-agent pipeline, triage queue | Mayor + Polecats, sling  | Issue->PR (original) | Cloud sandbox, fleet | Hierarchical delegation | Single issue->patch  | Interactive pair | Plan/Act + subagents | Boomerang orchestrator | Agentic loop + headless |
| **Verification**  | 5-tier automated                   | Human + problems view    | PR review            | Devin Review (auto)  | Event-source replay     | Best-of-N + verifier | Git commits      | Human per-action     | Role-based             | Self-verify + human PR  |
| **Multi-Repo**    | Cross-repo deps, threads           | Rigs + convoys           | Single repo          | Fleet across repos   | Single container        | Single repo          | Single repo      | VS Code workspace    | VS Code workspace      | xargs -P parallel       |
| **Trust Model**   | Tier 0-3 hierarchy (ADR-0008)      | Wasteland stamps (L1-L3) | PR approval          | PR + Devin Review    | Event audit trail       | None (academic)      | Human-in-loop    | Per-action approval  | Mode-scoped tools      | Graduated auto-approve  |
| **Cost**          | OSS + BYOK                         | OSS + BYOK               | Freemium (uncertain) | $20-500/mo + ACU     | OSS / $500/mo cloud     | OSS + BYOK           | OSS + BYOK       | OSS + BYOK           | OSS / $20+/mo cloud    | $20/mo + API            |
| **State**         | Dolt (version-controlled SQL)      | Git-backed beads         | Ephemeral            | Cloud state          | Event-sourced           | Ephemeral            | Git commits      | Session logs         | Session state          | Session + hooks         |
| **Model Lock-in** | None (Claude/Gemini/Qwen)          | None (9+ runtimes)       | Proprietary          | Proprietary          | None (any LLM)          | None (any LLM)       | None (any LLM)   | None (any provider)  | None (any provider)    | Anthropic only          |
| **IDE**           | None (CLI + MCP)                   | CLI (gt)                 | JetBrains            | Web UI               | Web UI                  | CLI                  | Terminal         | VS Code              | VS Code                | Terminal                |

______________________________________________________________________

## Strategic Observations

### Rosary's Unique Position

Rosary occupies a distinct niche: **cross-repo autonomous orchestration with structured verification and persistent state.** No other system combines:

1. **Dolt-backed version-controlled work tracking** with SQL queryability
1. **Multi-perspective pipeline** where different agent personas review the same work
1. **Thread-aware triage** that sequences related beads and detects file overlap
1. **Language-aware verification** (Rust: cargo check/test/clippy; Go: go vet/test/lint)
1. **Work decomposition** from ADR-level rationale down to atomic beads

The closest competitor architecturally is **Gas Town**, which shares the beads concept (Dolt-backed, same author inspiration), worktree isolation, and multi-agent dispatch. The Wasteland's federated trust network is the most advanced trust model in the space and is worth studying for rosary's future multi-team story.

### Gaps to Address

**Priority 1 — Adoption and Accessibility:**

- No IDE integration (most competitors live in VS Code or web UIs)
- No web dashboard for non-terminal users
- No cloud-hosted option for teams without infrastructure expertise

**Priority 2 — Execution Environment:**

- No sandboxed execution (Docker/container isolation for untrusted code)
- No browser agent for web research during development
- No event-sourced state with deterministic replay

**Priority 3 — Ecosystem:**

- No federated trust for multi-org collaboration (Gas Town Wasteland model)
- No formal benchmark results (SWE-bench scores would validate approach)
- No SDK for building on top of rosary (Claude Code has Python/TypeScript SDKs)

### Market Dynamics (March 2026)

The market is splitting into two tiers:

1. **IDE-embedded agents** (Cline, Roo Code, Aider, Claude Code): high adoption, human-in-the-loop, single-repo focused. Competing on UX and model flexibility.

1. **Autonomous orchestrators** (Rosary, Gas Town, Devin, OpenHands): lower adoption, higher automation ceiling, cross-repo potential. Competing on reliability and verification.

Rosary is positioned in tier 2 with the strongest verification pipeline and the most structured work decomposition. Gas Town has the strongest community story. Devin has the strongest enterprise sales motion. OpenHands has the strongest academic backing.

Claude Code straddles both tiers: it is an IDE-embedded agent (terminal) that, via headless mode and worktree hooks, can serve as the execution substrate for orchestrators like rosary. This makes Claude Code a runtime dependency more than a competitor — rosary dispatches Claude Code as one of its agent providers.

______________________________________________________________________

*Last updated: 2026-03-21*
