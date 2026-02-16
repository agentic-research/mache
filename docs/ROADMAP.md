# Roadmap

## Current State (as of Feb 2026)

**What's landed:**
- Schema-driven ingestion for JSON, SQLite, Go, Python, JavaScript, TypeScript, SQL, HCL/Terraform, YAML
- Two graph backends: `MemoryStore` (in-memory map) and `SQLiteGraph` (zero-copy SQL)
- Two mount backends: NFS (macOS default, `go-nfs`/`billy`) and FUSE (Linux default, `cgofuse`/`fuse-t`)
- Write-back pipeline: validate (tree-sitter) → format (gofumpt for Go, hclwrite for HCL/Terraform) → splice → surgical node update + ShiftOrigins (no re-ingest)
- Draft mode: invalid writes save as drafts, node path stays stable, errors via `_diagnostics/`
- Context awareness: virtual `context` files expose imports/globals to agents
- Cross-reference indexing: roaring bitmap inverted index for token → file lookups
- `callers/` virtual directory: exposes cross-references as browsable entries (NFS: graphFiles, FUSE: symlinks). Self-gating — only appears when callers exist
- `.query/` Plan 9-style SQL query directory with symlink results
- Go schema captures: functions, methods, types, constants, variables, imports
- Init dedup: same-name constructs get `.from_<filename>` suffixes (`engine.go:dedupSuffix`)
- FCA schema inference: `--infer` auto-generates topology from data via Formal Concept Analysis
- Greedy entropy inference: information-theoretic field scoring for better schema quality
- Virtual `_schema.json` at mount root exposing the active topology
- HotSwap graph with control block for live schema reload

**Known limitations:**
- Memory: ~2GB peak for 323K NVD records (1.6M graph nodes with string IDs)
- Write-back formatting is Go and HCL/Terraform only (other languages validate but don't auto-format)
- No offset-based readdir pagination (fuse-t requires auto-mode, see `fs/root.go:Readdir`)

## Near-Term: Construct Creation via FUSE

**Problem:** You can edit existing nodes but not create new ones. `mkdir {pkg}/functions/NewFunc` doesn't work.

**What exists:** `SourceOrigin` (`graph.go`) tracks which file owns each construct. The engine knows all files per package. Both backends already have `writeHandle`/`writeFile` tracking.

**What's needed:** A `Mkdir` + `Create` handler that:
1. Resolves the parent node's `SourceOrigin.FilePath` to find the target source file
2. Generates a stub from a template (e.g. empty function signature)
3. Appends to the source file (reuse `writeback/splice.go` with `EndByte` = file length)
4. Surgical update via `UpdateNodeContent`

**Pragmatic alternative:** Agents can write directly to source files (`echo >> file.go`) and mache re-ingests on the next writable file close. This works today without any changes.

## Medium-Term

- **Additional walkers** — TOML, Rust, and more tree-sitter grammars. Adding a grammar requires: a `smacker/go-tree-sitter` language binding + a file extension case in `engine.go:ingestFile` + a ref/context query in `engine_languages.go`
- **Additional formatters** — Python (black/ruff), TypeScript (prettier). Validation works for all tree-sitter languages; formatting needs per-language wiring in `writeback/format.go`

## Long-Term (ADR-Described, No Code)

- **Content-addressed storage** (ADR-0003) — Store data by hash, hard links for dedup
- **Layered overlays** (ADR-0003) — Docker-style composable layers for versioned views
- **SQLite virtual tables** (ADR-0004) — SQL queries over the projected filesystem
- **MVCC memory ledger** (ADR-0004) — Wait-free RCU + ECS for 10M+ entities, gated on profiling
