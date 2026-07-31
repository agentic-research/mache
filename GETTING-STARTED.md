# Getting started with mache

This guide walks you from install to a working MCP server in under five minutes, then through the things you'll likely want next: filesystem mount, write-back, schema inference, troubleshooting.

For the architectural picture, see [ARCHITECTURE.md](docs/ARCHITECTURE.md). For where the project is going, see [ROADMAP.md](docs/ROADMAP.md).

## Install

Build from source. Requires [Go 1.26+](https://go.dev/dl/), [Task](https://taskfile.dev/installation/), and network access for the exact SHA-pinned [ley-line-open](https://github.com/agentic-research/ley-line-open) backend:

```bash
git clone https://github.com/agentic-research/mache.git
cd mache
task build
task install   # installs mache and provisions the pinned Leyline release
```

**Platform notes:**

- **macOS** — `brew install go-task` for Task.
- **Linux** — [install Task](https://taskfile.dev/installation/). NFS mount requires `nfs-common` (`apt-get install nfs-common`).

To check the install:

```bash
mache version
```

### Installing without a checkout

`task install` needs this repository. If you got mache from a release asset or a
package manager, you have a binary and no Taskfile — so the binary provisions
itself:

```bash
mache install              # copy this binary to ~/.local/bin and fetch the pinned leyline
mache install --update-rc  # ...and add the PATH line to your shell rc (opt-in)
mache uninstall            # remove both again, reporting each path
```

`mache install` prints the `export PATH=...` line rather than editing your shell
rc unless you pass `--update-rc`, and it refuses to install over — or uninstall —
a Homebrew-managed mache, since brew's manifest would still claim the file.

The pinned leyline is cached **version-namespaced** under `~/.mache/bin`, which is
deliberately not put on PATH: a bare `leyline` in your shell would then mean
whatever PATH order decided, which is the exact skew this pin exists to prevent.
Reach the right one explicitly:

```bash
mache leyline path                          # absolute path, nothing else
mache leyline exec -- cdc enable --db x.db  # run it, version-correct by construction
```

## First run — MCP server

Running mache as an MCP server is **two decisions**: *what to point it at* (the source) and *how the client connects* (the transport). Get these right once and it just works.

### Decision 1 — the source: directory vs `.db`

`mache serve <source>` accepts either a **directory** or a **`.db` file**, and the choice is a real quality tradeoff:

| Source                        | Command                                                          | Parsing tier                                                                                                                                                   | Freshness                                                                                                  | Best for                                                      |
| ----------------------------- | ---------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| **Directory**                 | `mache serve .`                                                  | `leyline parse` → `_ast` → Mache's pure-Go `ASTWalker`. The exact pinned Leyline release is resolved from `PATH`, the local cache, or a SHA-verified download. | Live — re-parses as files change.                                                                          | The simplest source-code path.                                |
| **ley-line `.db` (snapshot)** | `leyline parse .` → `mache serve ./that.db`                      | semantic. `_ast` + `_lsp` tables make `find_callers`, `get_type_info`, `get_diagnostics` accurate **across all languages**.                                    | Static — reflects the code as of the last `leyline parse`. Re-run to refresh.                              | Rust/Python/TS, or any time you want compiler-grade accuracy. |
| **ley-line live (hot-swap)**  | `mache serve --control <block>` with the ley-line daemon running | semantic (same as above).                                                                                                                                      | Live — the daemon re-parses on edit and mache **hot-swaps** the graph on each generation bump (zero-copy). | The best of both: semantic accuracy *and* live updates.       |

Rule of thumb: **use a directory for the automatic base projection; use a pre-built Leyline `.db` when you want reproducible snapshot, LSP, or embedding enrichment; use the live daemon for hot-swap.** Pointing stdio at a Leyline-produced `.db` (for example, `mache serve --stdio ./code.db`) is correct and recommended. See [Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open) for which tools need which tables.

> `--control` is **optional** — you only need it for the live hot-swap tier above. Basic config never requires it.

### Decision 2 — the transport: HTTP daemon vs stdio

**HTTP (recommended — one daemon shared across all sessions):**

```bash
mache serve .                 # ← long-running daemon on localhost:7532 — KEEP THIS RUNNING
claude mcp add --transport http mache http://localhost:7532/mcp
```

> ⚠️ `mache serve` (HTTP) is a **foreground daemon** — it blocks the terminal and must stay alive for the client to connect. If the client reports `localhost:7532/mcp` **connection refused**, the daemon isn't running — that's the #1 first-run gotcha. The fix is a one-time `mache init --global`, which installs a per-user supervisor (**launchd** LaunchAgent on macOS, **systemd `--user`** unit on Linux) that starts the daemon at login and keeps it alive across restarts — no terminal to babysit. For an ad-hoc run instead, background it (`mache serve . &`) in a second terminal.

**Codex** uses the same Streamable HTTP endpoint:

```bash
codex mcp add mache --url http://localhost:7532/mcp
```

Or add the URL-only server entry to `~/.codex/config.toml`:

```toml
[mcp_servers.mache]
url = "http://localhost:7532/mcp"
```

Do not add `type = "sse"` or use a `/sse` URL. Modern Mache serves MCP
Streamable HTTP at `/mcp`, and Codex selects that transport from the `url`
field.

Codex merges user and trusted-project configuration by key. If a project
defines `mcp_servers.mache.url` while your user config still defines
`mcp_servers.mache.command` or `args`, the merged entry mixes HTTP and stdio
and startup fails with `url is not supported for stdio`. Replace the old
global registration atomically:

```bash
codex mcp remove mache
codex mcp add mache --url http://localhost:7532/mcp
```

Verify the effective configuration from the project that previously failed:

```bash
codex mcp list
```

**stdio (subprocess per session — no standing daemon to babysit):**

```bash
claude mcp add mache -- mache serve --stdio --path .
# or point at a leyline .db for the accurate tier:
claude mcp add mache -- mache serve --stdio --path . ./code.db
```

### Optional: CDC for a Mache-managed Leyline daemon

CDC is **off by default**. To enable it when Mache starts its own Leyline daemon,
add `--cdc` to the Mache command:

```bash
claude mcp add mache -- mache serve --stdio --cdc --path .
# equivalent direct command:
mache serve --stdio --cdc --path .
```

The flag is applied only when Mache starts a managed Leyline daemon. A pre-built
`.db` does not start one, and an externally managed daemon selected with
`--control` keeps its existing startup arguments. Start those external Leyline
daemons with Leyline's own `--cdc` flag if CDC is required; a daemon already
running cannot be reconfigured by Mache.

Mache asks the MCP client for its workspace roots before it starts scanning. `--path`
provides a safe fallback when a client cannot supply roots; pass one positional source
only for a snapshot such as `./code.db`.

Add `--scope user|project|local` to either `claude mcp add` to control where the registration is written (`local` = this project only, `user` = all your projects). Or write `.mcp.json` by hand:

```json
{
  "mcpServers": {
    "mache": {
      "command": "mache",
      "args": ["serve", "--stdio", "--path", "."]
    }
  }
}
```

**Claude Desktop** (stdio only; use an absolute binary path):

```json
{
  "mcpServers": {
    "mache": {
      "command": "/path/to/mache",
      "args": ["serve", "--stdio", "--path", "/path/to/code"]
    }
  }
}
```

Once connected, the agent can call `list_directory`, `read_file`, `find_callers`, `find_callees`, `find_definition`, `search`, `get_communities`, `get_overview`, `get_impact`, `get_architecture`, `get_diagram`, `write_file`, `find_smells`, and `resolve_ref`. With a ley-line-built `.db` (or the live daemon), three more light up: `semantic_search`, `get_type_info`, `get_diagnostics`.

See the [MCP Server section in ARCHITECTURE.md](docs/ARCHITECTURE.md#core-abstractions) for the full tool inventory and the [Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open) section for which tools require a `.db` built by ley-line-open.

## Cross-repo serve (`--mount`)

To span multiple codebases from one MCP endpoint, use repeatable `--mount NAME=PATH` flags:

```bash
mache serve --mount auth=./auth-svc --mount billing=./billing-svc
```

Each mount becomes a top-level virtual directory. `find_callers Validate` walks both repos and returns results annotated with their mount of origin:

```json
{
  "callers": [
    {"path": "auth/functions/AuthCaller/source", "mount": "auth"},
    {"path": "billing/functions/Charge/source",  "mount": "billing"}
  ]
}
```

Mounts compose any source `mache serve` accepts — directories, `.db` files, or a mix. `--mount` and a positional source are mutually exclusive (use one or the other).

See [ARCHITECTURE.md § Cross-repo serve](docs/ARCHITECTURE.md#cross-repo-serve---mount) for the full design. Cross-mount `find_callees` / `find_definition` / `find_callers` all resolve and annotate today; `search` and `get_impact` still emit the legacy single-string shape on cross-mount results.

## Mount as a filesystem (optional)

Beyond MCP, the graph can be exposed as a mounted directory tree — useful for `ls`, `cat`, shell scripts, or tools that work with files:

```bash
mache --infer -d ./src --writable /tmp/mache-src
```

Layout:

```
/tmp/mache-src/
  functions/
    HandleRequest/
      source        # the function body
      context       # imports, types visible to this scope
      callers/      # who calls this function
      callees/      # what this function calls
    ValidateToken/
      source
  types/
    Config/
      source        # type Config struct { ... }
  _project_files/
    README.md
    go.mod
```

Navigate by function name, not file path. `callers/` and `callees/` are virtual directories that appear only when references exist.

**Other mount examples:**

```bash
# Agent mode — generates PROMPT.txt for LLMs
mache --agent -d ~/my-project

# Mount a SQLite database directly (zero-copy, no ingestion)
mache --schema examples/nvd-schema.json --data results.db /tmp/nvd

# Hot-reload schema while mounted
mache --control /tmp/control.sock --infer -d ./src /tmp/mount
```

NFS is the only mount backend (FUSE was removed in v0.7.0; for FUSE today, use [ley-line-open's `leyline serve`](https://github.com/agentic-research/ley-line-open)).

## Write-back

With `--writable`, edits to `source` files go through a pipeline before touching the actual source on disk:

1. **Validate** — tree-sitter checks syntax
1. **Format** — gofumpt for Go, hclwrite for HCL/Terraform
1. **Splice** — atomic byte-range replacement in the source file
1. **Update** — node content updated in-place, no re-ingest

If validation fails, the write is saved as a draft. The node path stays stable; errors surface via `_diagnostics/`.

For the full pipeline design, see [ADR-0009: AST-Aware Write Pipeline](docs/adr/0009-ast-aware-write-pipeline.md).

## Schema inference

Mache auto-infers a topology from your source if you don't provide a schema (`--infer` is the default for source mounts). The inference pipeline:

1. **FCA** (Formal Concept Analysis) — reservoir-samples records, builds a concept lattice, projects to a `Topology`
1. **Greedy entropy** — information-theoretic field scoring picks identifier columns, temporal shards, leaf files
1. Schema-driven projection mounts the result

To inspect the inferred schema after mount:

```bash
cat /tmp/mache-src/_schema.json
```

Or pass an explicit schema if the inferred one isn't right:

```bash
mache serve --schema examples/go-schema.json ./src
```

See [ADR-0005](docs/adr/0005-fca-schema-inference.md) and [ADR-0008](docs/adr/0008-greedy-entropy-schema-inference.md) for the inference design.

## Troubleshooting

**`get_diagnostics` returns "no \_lsp table in database"** — the LSP-enrichment tools require the optional Leyline LSP pass. Either pre-enrich the `.db`, pass a `file` parameter to trigger live enrichment, or use the base tools over the `_ast` projection. The parser itself is still Leyline in every source-code mode. See [Interplay with ley-line-open](docs/ARCHITECTURE.md#interplay-with-ley-line-open).

**`find_smells` reports a missing `_ast` table** — the input is not a complete Leyline source projection. Rebuild from the source directory or point Mache at a Leyline-produced `.db`. Mache no longer has a standalone in-process parser fallback.

**Mount stuck or `umount` complains "device busy"** — see `mache unmount <mountpoint>` and the open-bead ergonomics in `mache-fsi`. macOS sometimes needs `diskutil unmount force`.

**NFS mount fails on Linux with "no such device"** — install `nfs-common` (`apt-get install nfs-common`).

**No pinned Leyline binary is available** — run `task leyline:ensure`. It refreshes `~/.mache/bin/leyline` from the exact published release after verifying the platform SHA-256. `MACHE_NO_LEYLINE=1` intentionally disables that download and therefore cannot be used for source projection.

## Where to next

- [ARCHITECTURE.md](docs/ARCHITECTURE.md) — graph backends, write pipeline, virtual directories, ley-line-open interplay
- [ROADMAP.md](docs/ROADMAP.md) — what's stable, what's next, what's long-term
- [CONTRIBUTING.md](CONTRIBUTING.md) — how to send a PR
- [docs/adr/](docs/adr/) — Architectural Decision Records
- [docs/reference/competitive-landscape.md](docs/reference/competitive-landscape.md) — what mache builds on + comparison with other tools in the space
- [docs/reference/arena.md](docs/reference/arena.md) — the capability-arena harness
