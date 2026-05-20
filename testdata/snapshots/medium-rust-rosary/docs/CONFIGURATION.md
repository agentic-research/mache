# Configuration

Rosary uses TOML configuration files. Settings are merged from multiple sources (highest priority first):

1. `$RSRY_CONFIG` (env var) — explicit config path
1. `~/.rsry/config.toml` — global user config (repos, Linear, GitHub, backend)
1. `./rosary.toml` — project-level config (overrides for this repo)
1. `./rosary-self.toml` — self-management config (dogfooding)

## Sections

### `[[repo]]` — Repository Registration

Register repos for scanning, dispatch, and sync.

```toml
[[repo]]
name = "rosary"          # Display name
path = "~/remotes/art/rosary"  # Absolute or ~ path
lang = "rust"            # Language hint (auto-detected if omitted)
self = true              # This repo IS rosary (dogfooding flag)
```

Required fields: `name`, `path`. Repos without `.beads/` directories are skipped during scan.

### `[linear]` — Linear Integration

Bidirectional sync with Linear (issue tracker).

```toml
[linear]
team = "AGE"                    # Linear team key
api_key = "lin_api_..."         # Or set LINEAR_API_KEY env var
webhook_secret = "lin_wh_..."   # For webhook verification

[linear.states]
# Override bead status → Linear state mapping
# dispatched = "Working"
# verifying = "Peer Review"

[linear.phases]
# Map phase numbers to Linear project names
# "1" = "Phase 1: Foundation"
# "2" = "Phase 2: Integration"
```

### `[backend]` — Orchestrator State Store

Cross-repo state: decades, threads, pipeline tracking, dispatch history. Required for thread-aware features (thread/decade MCP tools, thread-aware triage, Linear sub-issue projection).

```toml
[backend]
provider = "dolt"                # Only "dolt" supported currently
path = "~/.rsry/dolt/rosary"    # Database directory (auto-initialized)
```

If omitted, hierarchy features degrade gracefully — beads work but threads/decades are unavailable.

### `[github]` — GitHub Integration

PR creation from the dispatch pipeline.

```toml
[github]
token = "gho_..."           # Fine-grained PAT
owner = "agentic-research"  # Default org for PRs
base = "main"               # Default base branch
auto_pr = false             # Auto-create PR on pipeline completion
```

### `[compute]` — Compute Provider

Where agents run. Default is local subprocesses.

```toml
[compute]
backend = "local"    # "local" or "sprites"

[compute.sprites]
token_env = "SPRITES_TOKEN"     # Env var for API token
cpu = 4                         # Default CPU cores
memory_mb = 8192                # Default memory
checkpoint_on_complete = true   # Snapshot on completion
fallback_to_local = true        # Fall back if sprites fails
network_allowlist = ["github.com", "crates.io"]
```

### `[dispatch]` — Pipeline Behavior

```toml
[dispatch]
provider = "claude"              # Default: "claude", "gemini", "acp"
adversarial_provider = "gemini"  # Provider for review phases
max_concurrent = 3               # Max parallel dispatches

[dispatch.binaries]
claude = "/Users/me/.local/bin/claude"     # Absolute path (avoids PATH issues)
acp = "/opt/homebrew/bin/claude-agent-acp" # ACP protocol agent
```

#### Authentication for dispatched agents

**This is critical.** The rsry HTTP server runs as a launchd service which cannot
access macOS Keychain for OAuth tokens. Dispatched agents need auth credentials
passed via environment variable or config.

Rosary resolves the token at dispatch time in this priority order:

1. `CLAUDE_CODE_OAUTH_TOKEN` environment variable
2. `ANTHROPIC_API_KEY` environment variable
3. `.envrc` in the agent's working directory
4. `.envrc` in the git repo root (for worktree dispatches)
5. `dispatch.anthropic_api_key` in `~/.rsry/config.toml` *(wasteland / hosted rigs)*

Without any of these, dispatched agents fail with "Not logged in".

**Option A — per-repo `.envrc` (local development):**

1. Run `claude setup-token` to generate a long-lived OAuth token (valid 1 year)
2. Add to your repo's `.envrc`:
   ```bash
   export CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...
   ```
3. Run `direnv allow` in the repo directory

**Option B — global config (wasteland / hosted rigs):**

For remote repos cloned on demand (wasteland), there is no per-repo `.envrc`.
Set the token once in `~/.rsry/config.toml`:

```toml
[dispatch]
anthropic_api_key = "sk-ant-oat01-..."  # or ANTHROPIC_API_KEY value
```

This is the recommended pattern for hosted rigs — mirrors how `claude-code-action`
passes `CLAUDE_CODE_OAUTH_TOKEN` as the `anthropic_api_key` GHA input.

**ACP provider:** `ANTHROPIC_API_KEY` (Console API key) or the OAuth token work.
Both are passed as `CLAUDE_CODE_OAUTH_TOKEN` to the ACP agent subprocess.

### `[[plugin]]` — Pipeline Plugins

Plugins extend rosary along a `kind` axis. Defined in `~/.rsry/plugins/*.toml`,
`<repo>/.rosary/plugins/*.toml`, or inline in any config file.

```toml
[[plugin]]
name = "assay-coverage"
kind = "hook"                       # hook (default) | mcp | dispatch | state_sink
hook = "pipeline.verify"            # which pipeline event (hook kind only)
command = ["assay", "verify", "--threshold", "0.8", "--format", "json"]
```

`hook` plugin events:

| Hook              | When                           | Use case                                    |
| ----------------- | ------------------------------ | ------------------------------------------- |
| `pipeline.triage` | Before dispatch                | Skip / reroute beads                        |
| `pipeline.verify` | After agent completes          | Verdict pass / fail / partial               |
| `pipeline.close`  | On bead close                  | Notify, archive, post-mortem                |
| `assay.scan`      | On `rsry scan --assay`         | Report stale refs → P3 chore beads          |

`pipeline.verify` plugins may include `coverage: <0..1>` in their JSON output.
When a bead's `success_criteria.doc_coverage_min` is set, rosary fails the gate
if the plugin reports coverage below the threshold.

`dispatch` plugins implement an alternative `AgentProvider` (e.g. local model
runner). `mcp` plugins expose an outbound MCP server that rosary connects to as
a client. `state_sink` plugins mirror bead state to an external system.

### `[http]` — HTTP Transport

For `rsry serve --transport http`. Exposes MCP over Streamable HTTP + webhook receiver.

```toml
[http]
port = 8383

[http.tunnel]
provider = "cloudflare"      # Tunnel for public access
domain = "rsry.example.com"  # Custom domain (optional)
account_id = "..."
zone_id = "..."
token_env = "CF_API_TOKEN"
```

## Environment Variables

| Variable                    | Purpose                        | Config equivalent                    |
| --------------------------- | ------------------------------ | ------------------------------------ |
| `RSRY_CONFIG`               | Config file path               | —                                    |
| `CLAUDE_CODE_OAUTH_TOKEN`   | Agent auth (OAuth token)       | `[dispatch] anthropic_api_key`       |
| `ANTHROPIC_API_KEY`         | Agent auth (API key)           | `[dispatch] anthropic_api_key`       |
| `LINEAR_API_KEY`            | Linear API key                 | `[linear] api_key`                   |
| `LINEAR_TEAM`               | Linear team key                | `[linear] team`                      |
| `LINEAR_WEBHOOK_SECRET`     | Webhook signing                | `[linear] webhook_secret`            |
| `SPRITES_TOKEN`             | Sprites compute API token      | `[compute.sprites] token_env`        |
| `GITHUB_TOKEN`              | Auth for `rsry sync --github`  | `[github] token`                     |

## File Locations

| File                                  | Purpose                                        |
| ------------------------------------- | ---------------------------------------------- |
| `~/.rsry/config.toml`                 | Global config                                  |
| `~/.rsry/plugins/*.toml`              | Global plugin discovery                        |
| `~/.rsry/dolt/rosary/`                | Backend state DB (decades, threads, pipelines) |
| `~/.rsry/worktrees/{repo}/{bead-id}/` | Agent worktree isolation                       |
| `{repo}/.beads/`                      | Per-repo bead database (Dolt)                  |
| `{repo}/.beads/dolt-server.port`      | Dolt server port                               |
| `{repo}/.beads/metadata.json`         | Database name + settings                       |
| `{repo}/.rosary/plugins/*.toml`       | Per-repo plugin discovery                      |
| `{repo}/notes/<scope>/.recipients`    | age recipients allowed to decrypt that scope   |
| `{repo}/notes/<scope>/*.age`          | Encrypted notes (rotated via `rsry notes rotate`) |
| `./rosary.toml`                       | Project-level config                           |
| `./rosary-self.toml`                  | Self-management config                         |
