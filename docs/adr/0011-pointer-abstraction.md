---
title: "ADR-0011: Pointer Abstraction — Filesystems, Graphs, CAS, Git, Diffs Are All the Same Thing"
status: Proposed
date: 2026-04-29
tags: [architecture, abstraction, pointer, cas, git]
---

## Context

Mache today has a graph (`Node`, `Children`, `ContentRef`), a refs index (`token → []nodeID`), an LSP enrichment table (`_lsp_refs`, `_lsp_defs`), schema-driven projections (selectors → templates), and a write-back pipeline that splices source bytes. The MCP tool surface presents these as separate primitives: `list_directory`, `read_file`, `find_callers`, `find_callees`, `find_definition`, `search`, `get_communities`.

Several proposed-but-unbuilt directions stretch this:

- **ADR-0003** (CAS + Layered Overlays) — content-addressed blob store, paths as overlays.
- **ADR-0007** (Git Object Graph as FS Projection) — commits as directories, trees as symlinks.
- **`mache-bsq`** — `detect_changes`: git diff → affected AST nodes → blast radius via callers BFS.
- **`mache-cbf644`** — `mache_embedding_similarity`: embedding-space neighbor lookup as a tool.
- **`mache-ok2`** — `trace/` virtual dir: transitive call-path traversal as a navigable directory.
- **`mache-2f0075`** / **`mache-d45da8`** / **`mache-ae2432`** — R2 storage backends.
- **`mache-d44509`** — D1 backend.
- **`rsry-b003eb`** — transactional multi-node writes for complex refactors.
- **`rsry-b0040c`** — sticky / zombie node handles to preserve identity across renames.
- **`rsry-b003dd`** — file-level imports as a virtual node for semantic editing.

Each of these has been treated as a separate feature requiring separate machinery. They aren't.

## Decision

Adopt **Pointer** as the unifying abstraction across mache's existing graph model, the proposed CAS/git directions, and the listed unbuilt features.

A pointer is anything that resolves — given a graph context — to either bytes, more pointers, or both. The graph is a network of pointers; mache's job is to resolve them on demand.

```go
// Pointer is the unifying interface. Every navigable thing in mache
// — paths, tokens, SHAs, ranges, records, refs — is a Pointer.
type Pointer interface {
    // Resolve dereferences once. May return another Pointer (for
    // symbolic refs that indirect) or self (for terminal pointers).
    Resolve(g Graph) (Pointer, error)

    // Read dereferences to bytes when the pointer is terminal
    // (blob, file, rendered template). Returns ErrNotTerminal otherwise.
    Read(g Graph) (io.Reader, error)

    // Children lists outgoing edges. For a directory, child node IDs.
    // For a token, callers/callees. For a commit, parent + tree.
    // Returns empty slice (not error) when there are no edges.
    Children(g Graph) ([]Pointer, error)
}
```

### Mache today, re-read as pointers

| Mache concept                                      | Pointer kind               | Notes                                 |
| -------------------------------------------------- | -------------------------- | ------------------------------------- |
| `Node.ID` (e.g. `pkg/funcs/Foo`)                   | path                       | mutable; `Resolve` returns the `Node` |
| `Node.Children []string`                           | set of path pointers       | terminal once resolved to nodes       |
| `Node.Ref *ContentRef{DBPath, RecordID, Template}` | deferred CAS-style         | `Read` lazily fetches + renders       |
| `find_callers(token)`                              | symbolic-ref token pointer | `Children` returns referencing nodes  |
| `_lsp_refs(token, file, line)`                     | location pointer           | `Read` returns the source span        |
| Schema selector + template                         | template pointer           | record + template → path tree         |
| `SourceOrigin{File, Start, End}`                   | byte-range pointer         | `Read` returns those bytes            |

Nothing in this model is invented. It's mache's existing graph, named for what it always was.

### Concrete pointer kinds (specializations)

These are the Pointer types we'd implement, each subsuming one or more current-or-proposed features:

1. **Path** — `pkg/funcs/Foo`. Mutable; current `list_directory`/`read_file` operate on these.
1. **Token** — `Foo`. Symbolic; current `find_callers`/`find_callees`/`find_definition` operate on these.
1. **SHA** — `sha256:abc123` or git SHA. Immutable, content-addressed. Subsumes ADR-0003 (CAS) and ADR-0007 (git-as-fs).
1. **Range** — `from..to`, where `from` and `to` are themselves Pointers. The pointer's content is the *delta*. Subsumes `mache-bsq` (`detect_changes`).
1. **Record** — `db:table:id`. The SQLite/D1/R2 row backing a node. Subsumes `mache-d44509` (D1), `mache-2f0075` / `mache-d45da8` (R2), and the existing `ContentRef`.
1. **Ref** — symbolic name. `HEAD`, `main`, `pr:208`, file path. Mutable indirection; resolves to whatever the name currently points at.
1. **Trace** — `from →* to`, the transitive path through edges. Subsumes `mache-ok2`.
1. **Embedding** — vector + radius. Subsumes `mache-cbf644` (`mache_embedding_similarity`).

These compose. A PR pointer is `Range{from: branchRef, to: branchRef}`. A "files at commit" pointer is `Children(SHA{...})`. Embedding nearest-neighbors return `[]Path` pointers. A schema selector is a Template pointer that resolves to a tree of Path pointers.

### What the unification kills

Three architectural problems collapse:

1. **CAS / git / PR / range** stop being separate features. They're pointer types under one model.
1. **The schema engine** becomes "declare the pointer shape" — what selectors exist, what they deref to. Today template rendering and topology declaration are tangled; pulled apart they're cleaner.
1. **Read/write symmetry.** Every read is `Resolve` then `Read`; every write is a pointer rewrite (`Path → newContent`, splicing the byte range). Today read and write paths are separate code; under this model they're inverses.

### Why this isn't already obvious

Filesystems are pointer systems (path → bytes/dir). Git is a pointer system (SHA → blob/tree/commit). Programming languages are pointer systems (variable name → value, function name → code). Mache projects any pointer system *as* a filesystem because filesystems already speak the language.

We didn't see it because we built mache one feature at a time, naming each one after its specific shape: "graph node", "ref", "callers", "schema selector". Different vocabularies for the same thing.

## Consequences

### Positive

- **One model, many backends.** R2, D1, git, CAS, SQLite — each is "implement Pointer for this storage." Today each is a separate epic.
- **Composability.** Range pointers contain other pointers. Trace pointers chain. Embedding pointers return Path pointers. New combinations land for free.
- **Cacheability.** Any pointer can be content-addressed (path+commit, hash, etc.). Projection results memoize by pointer-tuple identity.
- **Distribution.** Pointers + groups can be replicated across machines (git's distribution model, generalized). Underpins hosted mache (ADR-0010) and persistent R2 mode (`mache-ae2432`).
- **MCP tool taxonomy clarifies.** Today: 16 ad-hoc tools. Under this model: a small set of pointer-shaped tools (resolve, read, children) plus rule/inference tools (find_smells, get_communities) that operate on pointer-shaped inputs.

### Negative

- **Every existing call site speaks the legacy vocabulary.** Migrating to a Pointer interface is a substantial refactor across `internal/graph/`, `cmd/serve_handlers.go`, `internal/ingest/`. Not a single PR.
- **The interface design is untested.** This ADR is a framing claim, not a working implementation. The actual interface shape will move once we try to fit a non-trivial pointer kind (Range, Embedding) through it.
- **Risk of over-abstraction.** A Pointer interface with three methods sounds clean; the real implementations carry data shape (DBPath, byte ranges, templates, hash bytes, range bounds) that may not factor cleanly into a single type.
- **No immediate user-visible change.** The first PRs implementing this will refactor existing code to fit the new model. Value lands only when the second wave (CAS, git, range, embedding) builds on the substrate.

## Path forward

Not committing to an implementation timeline in this ADR. Likely sequence when we do start:

1. **Define `Pointer` and a couple of trivial implementations** (`PathPointer`, `TokenPointer`) that shadow the existing graph methods. Don't migrate callers yet.
1. **Implement one new pointer kind** that doesn't exist today — most likely **`Range`** (since `mache-bsq` `detect_changes` already has demand). This proves the interface holds under a non-trivial case.
1. **Migrate one MCP tool** at a time — start with `find_callers` (smallest surface) to read via Pointer. Keep the legacy code path around behind a build tag until the migration is complete.
1. **Add `SHA` / `Record` pointers** for the CAS and R2/D1 backend work.
1. **Retire the legacy code paths** when every caller is migrated.

## Relation to existing ADRs and beads

This ADR generalizes:

- **ADR-0003** (Content-Addressed Storage & Layered Overlays) — becomes "implement `SHA` Pointer + path-as-overlay"
- **ADR-0007** (Git Object Graph as FS Projection) — becomes "implement `SHA` Pointer for git's object format, plus `Ref` for branch/tag, plus `Range` for diffs"

Beads that become specializations of this:

- `mache-bsq` (detect_changes) → `Range` pointer
- `mache-ok2` (trace/ virtual dir) → `Trace` pointer
- `mache-cbf644` (embedding_similarity) → `Embedding` pointer
- `mache-2f0075` / `mache-d45da8` / `mache-ae2432` / `mache-d44509` (R2 / D1 backends) → `Record` pointer with new storage backends
- `rsry-b0040c` (sticky/zombie node handles across renames) → `Ref` pointer with stable indirection
- `rsry-b003dd` (file-level imports as virtual node) → another `Path` pointer kind under the schema
- `rsry-b003eb` (transactional multi-node writes) → atomic pointer-rewrite primitive

These shouldn't be acted on individually until the substrate exists. Filed `mache-XXXX` as a tracking parent if/when we commit to the implementation.

## Caveat

This ADR is a *framing* claim. It says "look at mache this way" — it doesn't say "do these PRs now." The right next step after merging this is *not* to start refactoring the graph package. The right next step is to live with the framing for a while, see what other backlog items look different through this lens, and bring it back when we have a concrete reason (the most likely trigger: someone landing a non-trivial new pointer kind and finding the existing abstractions don't fit).

If after a month nothing has called for it, we should question whether the abstraction is real or just clean-on-paper.
