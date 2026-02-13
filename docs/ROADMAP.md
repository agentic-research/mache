# Roadmap

## Current State (as of Feb 2026)

**What's landed:**
- Schema-driven ingestion for JSON, SQLite, Go, Python sources
- Two graph backends: `MemoryStore` (in-memory map) and `SQLiteGraph` (zero-copy SQL)
- FUSE bridge with read + write support (tree-sitter sources only)
- Write-commit-reparse: splice → goimports → re-ingest on file close
- Go schema captures: functions, methods, types, constants, variables, imports
- Init dedup: same-name constructs get `.from_<filename>` suffixes (`engine.go:dedupSuffix`)
- Parallel SQLite ingestion: 323K NVD records in ~6s (worker pool in `engine.go:ingestSQLiteStreaming`)
- FCA schema inference: `--infer` auto-generates topology from data via Formal Concept Analysis (~33ms for 1.5K KEV records)
- Cross-reference indexing: roaring bitmap inverted index for token → file lookups
- Virtual `_schema.json` at mount root exposing the active topology

**Known limitations:**
- fuse-t NFS translation bottleneck: ~8s for `ls` on 30K+ entry dirs (cookie verifier invalidation)
- Memory: ~2GB peak for 323K NVD records (1.6M graph nodes with string IDs)
- Write-back is Go-only (Python tree-sitter captures exist but no goimports equivalent wired)
- No offset-based readdir pagination (fuse-t requires auto-mode, see `fs/root.go:Readdir`)

## Near-Term: Construct Creation via FUSE

**Problem:** You can edit existing nodes but not create new ones. `mkdir {pkg}/functions/NewFunc` doesn't work.

**What exists:** `SourceOrigin` (`graph.go`) tracks which file owns each construct. The engine knows all files per package. `MacheFS` already has `writeHandle` tracking in `fs/root.go`.

**What's needed:** A `Mkdir` + `Create` FUSE handler in `fs/root.go` that:
1. Resolves the parent node's `SourceOrigin.FilePath` to find the target source file
2. Generates a stub from a template (e.g. empty function signature)
3. Appends to the source file (reuse `writeback/splice.go` with `EndByte` = file length)
4. Re-ingests via `Engine.Ingest` (same as write-back Release handler)

**Pragmatic alternative:** Agents can write directly to source files (`echo >> file.go`) and mache re-ingests on the next writable file close. This works today without any FUSE changes.

## Near-Term: Cross-File References (Callers/Usages)

**Problem:** The graph shows what a file *defines* but not who *uses* it. `{pkg}/functions/Foo/callers/` doesn't exist.

**What exists:** Tree-sitter can already find call sites with queries like:
- `(call_expression function: (identifier) @callee) @scope`
- `(call_expression function: (selector_expression field: (field_identifier) @callee)) @scope`

The `SitterWalker` (`sitter_walker.go`) and `OriginProvider` interface already support capturing these.

**What's needed:** A **post-ingestion pass** in `Engine` that:
1. Walks all ingested file nodes to find call sites (second tree-sitter query pass)
2. Builds an inverted index: `callee_name → [(source_file, call_site_origin)]`
3. Injects synthetic `callers/` directory nodes into the graph via `Store.AddNode`
4. This is a new method on Engine (e.g. `Engine.BuildCrossRefs`) called after `Ingest` completes

**Key design question:** File↔construct ownership. `SourceOrigin` currently maps one construct → one file. Cross-refs need the reverse: one construct → many call sites across files. The graph supports this (nodes are just IDs + children), but the schema format (`examples/go-schema.json`) would need a way to express "post-ingestion derived nodes" vs "direct query nodes."

## Medium-Term

- **Additional walkers** — YAML, TOML, HCL, more tree-sitter grammars (TypeScript, Rust). Adding a grammar requires: a `smacker/go-tree-sitter` language binding + a file extension case in `engine.go:ingestFile` + an example schema
- **Go NFS server** — Replace fuse-t's NFS translation layer for full control over caching, pagination, and large directory performance. Would eliminate the 30K+ dir bottleneck

## Long-Term (ADR-Described, No Code)

- **Content-addressed storage** (ADR-0003) — Store data by hash, hard links for dedup
- **Layered overlays** (ADR-0003) — Docker-style composable layers for versioned views
- **SQLite virtual tables** (ADR-0004) — SQL queries over the projected filesystem
- **MVCC memory ledger** (ADR-0004) — Wait-free RCU + ECS for 10M+ entities, gated on profiling
