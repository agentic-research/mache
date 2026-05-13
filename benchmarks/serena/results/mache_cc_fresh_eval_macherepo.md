# Mache vs Built-ins — Fresh-Context Eval on the Mache Repo

**Methodology:** Serena's vendored one-shot evaluation prompt
(`benchmarks/serena/upstream/010_evaluation-prompt.md`), with "Serena" → "mache"
substitutions and the read-only scope adjustment (edit-side tasks 7–13, 15–17
classified out-of-scope per Serena's own taxonomy). Corpus is mache itself
(~28K LOC Go, 268 dirs in the projection). Evaluator entered with no prior
knowledge of mache's surface — no CLAUDE.md, no AGENTS.md, no memory file was
consulted before tool exploration.

______________________________________________________________________

## 1. Headline: what mache changes

On this corpus, with this projection, mache produces **one clearly positive
capability** and several **neutral-or-negative** ones relative to Read/Grep/Glob:

- **(a) Adds capability:** `find_callers` over the pre-baked reference index
  gives a clean, scope-filtered list of method/function-call sites with no
  doc-comment or string-literal noise. This is genuinely useful when you want
  "who *calls* this," and it returns ~150–190 entries on common helpers in one
  call with no per-file regex. Frequency: high. Value/hit: medium.
- **(b) Applies but offers no improvement:**
  - **File-level structural overview** (`list_directory` on a .go file) dumps
    `comment_NN` and `method_declaration_NN` entries without symbol names. A
    single `grep -nE '^(func|type|var|const) '` returns a richer outline with
    line numbers and signatures.
  - **Symbol-body retrieval** (`find_definition` → read) is broken for the
    obvious case: `find_definition GetCallers` returns "not found" even with
    `fuzzy=true`, despite eleven receivers defining that method in the tree.
    `find_definition Topology` and `Graph` likewise fail. To get a body the
    agent must enumerate `method_declaration_NN/field_identifier` files until
    it matches, which costs N tool calls vs. one targeted `Read`.
  - **Free-text / variable-name search:** `find_callers ErrNotFound` → `[]`;
    `search %ErrNotFound%` → `[]`; grep returns 85 hits. The refs index only
    captures function/method-call expressions, not type references or
    package-level variable reads.
- **(c) Out of scope for mache here:**
  - All edit-side tasks (mache is read-only on this surface).
  - `get_type_info` / `get_diagnostics` (no LSP tables baked into this
    projection — they error and tell you to run ley-line `enrich-lsp`).
- **(d) Negative delta where mache claims strength:**
  - `find_callers` misses **type references**. `find_callers Topology` → `[]`
    despite 141 grep hits for `api.Topology`/`Topology{`. Same story for
    `SocketClient` (26 grep hits, mache 0). Calling find_callers on a type
    name silently returns "nobody uses this" — that is a correctness hazard,
    not just a frequency miss.
  - `find_callees` returns `[]` for `SendOp` (line 260 in
    `internal/leyline/socket.go`) even though the method visibly invokes
    `c.sendRaw`, `json.Unmarshal`, and `fmt.Errorf`. Resolution of stdlib
    and receiver-method calls is missing.
  - `get_architecture` and `get_communities` produce 486K–720K-char payloads
    on a 28K-LOC repo even with `summary=true`, dominated by per-call-site
    construct paths. Both blow past the response budget.
  - `role=definition` filter on `search` is non-functional in this projection
    (returns `[]` for every pattern tried).

**Verdict.** Mache's `find_callers` for function/method tokens is the one
durable win on this corpus; everything else is at parity or worse than
`Grep` + `Read` for fresh-context exploration of this repo.

______________________________________________________________________

## 2. Added value and differences by area

| #   | Area                                                                                    | Δ vs built-ins                                                                                                                                                                                                                                                                                                                                   | Frequency  | Value/hit                                                                                                           |
| --- | --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------- | ------------------------------------------------------------------------------------------------------------------- |
| 1   | **Method/function call-site lookup** (`find_callers` on a function or method name)      | **Positive.** Returns clean call sites with no doc/string noise; results re-key into the same construct paths. Grep needs `--include='*.go'` + post-filtering for definition vs. use.                                                                                                                                                            | High       | Medium — saves 1 grep + manual filtering, especially on common names.                                               |
| 2   | **Type-reference lookup** (`find_callers` on a type)                                    | **Negative.** Silently returns `[]` for `Topology` (141 grep hits) and `SocketClient` (26). Agent who trusts the answer concludes "no callers" wrongly.                                                                                                                                                                                          | Medium     | High per-occurrence (correctness, not speed).                                                                       |
| 3   | **File overview / find a method by name** (`find_definition`, `list_directory file.go`) | **Negative.** `find_definition` fails for `GetCallers`, `Graph`, `Topology`, `MemoryStore.GetCallers`, `SQLiteGraph.GetCallers`. `list_directory` on a .go file emits `comment_NN` × ~158 plus unnamed `method_declaration_NN` × 40 — must round-trip to read each `field_identifier`. Grep with anchored regex returns the outline in one call. | High       | High — affects nearly every "find this method's body" workflow.                                                     |
| 4   | **Architectural overview** (`get_architecture`, `get_communities`)                      | **Negative for fresh context.** Output is dominated by `require.NoError`-style aggregate clusters (community 0 = 2089 NoError sites). Even `summary=true`, output exceeds the 25K-token response limit. A reader on a fresh repo cannot consume this.                                                                                            | Low-Medium | Low — current shape isn't a high-signal entry point.                                                                |
| 5   | **Free-text / magic-string search**                                                     | **Negative (mache not designed for it, declares so).** `search %ErrNotFound%` → `[]`. Grep wins.                                                                                                                                                                                                                                                 | Medium     | Low — Serena's prompt classifies this as out-of-scope, but mache's `search` *is* offered for this, so it's notable. |
| 6   | **Non-code file read**                                                                  | **Neutral.** `read_file Taskfile.yml` errors with "is a directory"; YAML/JSON live as projection trees. Use Read.                                                                                                                                                                                                                                | Medium     | n/a                                                                                                                 |
| 7   | **External-dependency signatures**                                                      | **Neutral here.** `get_type_info sql.Open` errors absent ley-line LSP enrichment; `go doc database/sql.Open` works in one shell call. Where LSP tables *are* present (ADR-mentioned), mache wins.                                                                                                                                                | Low        | High when applicable.                                                                                               |

**Verdict.** Mache adds one high-frequency win (function-call discovery) and one
high-frequency loss (broken `find_definition` + dotted name-paths + missing
type-ref capture); the net for an unfamiliar Go codebase tilts slightly negative.

______________________________________________________________________

## 3. Detailed evidence, grouped by capability

### 3.1 Top-level repo overview (Task 1)

**Mache:** 1 call (`get_overview`) → 2,884-token payload listing 38 top-level
entries with child counts, total_dirs=268, total_files=18, and a `_usage`
section advertising other tools. Useful: it immediately reveals the top-level
package layout *and* that this repo carries 18 root-level loose files and 28
items under `_agent_log/`.

**Built-in:** 2 calls — `ls -F` (~50 lines) plus `find . -maxdepth 2 -type d`
(~50 lines) — and one Read of `main.go`. Comparable content. `ls -F` also
surfaces non-projected files (binaries, `.lock`, `.pid`, generated PNGs) which
mache hides; mache's filtered view is cleaner.

**Verdict:** Slight positive for mache — single shot, includes child counts;
~equal content otherwise.

### 3.2 Architectural overview (Task 1 cont.)

**Mache:** `get_architecture` returned 720,868 chars (forced into a tool-result
spill file). `most_referenced` is dominated by stdlib + testify
(`NoError` 2089, `require.NoError` 2053, `Equal` 971, `len` 875, `Errorf` 633…).
`dependency_layers` enumerates every construct ID per layer, exploding payload.
`key_abstractions` was `null`.

**Built-in:** No direct equivalent — read `CLAUDE.md` (banned) or skim a few
top-level packages.

**Verdict:** Negative for mache as currently shaped — the data is real but
unconsumable from a fresh-context agent: 720K chars, no abstraction layer, and
the top entries are noise that any project would produce.

### 3.3 Large-file structural overview (Task 2)

Target: `internal/graph/graph.go` (1331 LOC).

**Mache:** `list_directory internal/graph/graph.go` returned a JSON array of
~200 entries: `comment_0` … `comment_157`, then `function_declaration_0…7`,
`method_declaration_0…39`, `type_declaration_0…10`, `var_declaration_0…2`,
`import_declaration`, `package_clause`. None of the construct dirs carry the
symbol name in the listing — you must `read_file <id>/field_identifier` (or
`identifier`) to find out which method is which.

Concrete next step on mache side: to find `GetCallers`, batch-read 40
`field_identifier` files. Cost: 1 list + 1 batch read (40 paths). Output:
~3KB.

**Built-in:** `grep -nE '^(func|type|var|const) ' internal/graph/graph.go`
returned 70 lines of structured signatures with line numbers. The same grep
answers "which line is GetCallers?" and "what's the receiver?" in one shot.
Cost: 1 call. Output: 70 lines.

**Verdict:** Negative for mache. The projection scatters one file across
~200 construct dirs without exposing names, while a single anchored grep
produces a strictly richer outline.

### 3.4 Retrieve a method body without reading the file (Task 3)

Target: `MemoryStore.GetCallers` (line 753 of `internal/graph/graph.go`).

**Mache call chain:**

1. `find_definition GetCallers` → `not found`.
1. `find_definition GetCallers fuzzy=true` → `not found`.
1. `list_directory internal/graph/graph.go` → identifies 40
   `method_declaration_NN` candidates with no names.
1. `read_file method_declaration_24/field_identifier` → `"GetCallers"` (hit
   by luck; would have batched in reality).
1. `read_file method_declaration_24/block/statement_list` → error "is a
   directory."
1. `list_directory method_declaration_24/block/statement_list` → 7
   sub-categories (`return_statement`, `if_statement`, `for_statement`, …).
   No path returns the assembled method *source*.

Net: mache can name the method dir but cannot reconstitute the method body as
a single text payload through the documented tools. To rebuild source you'd
have to recursively descend and concatenate — work the built-in tools do not
need.

**Built-in call chain:**

1. `Read internal/graph/graph.go offset=753 limit=20` → 20 lines including
   the entire 16-line method body. One call.

**Verdict:** Strong negative for mache on this task — the projection forfeits
the one thing semantic tools should do well (target-and-extract a single
symbol's body).

### 3.5 Find all references for a non-trivial symbol (Task 4)

Target: `NewMemoryStore`.

**Mache:** 1 call. `find_callers NewMemoryStore` → ~190 construct paths, all
genuine `call_expression` nodes, zero false positives from comments/strings.
Test-helper noise is fully present (this repo's helpers wrap NewMemoryStore
heavily), but that's accurate.

**Built-in:** 1 call. `grep -rn 'NewMemoryStore' --include='*.go' .` → 214
lines with file:line:source. To match mache's filtered "real calls only" set,
you'd post-filter out the 2 definition lines and any string-literal hits —
trivial here, ~one minute of awk in practice.

Recall: similar (190 vs 214). Precision: mache higher (refs index excludes
comments/strings/docs); built-in shows you the call context for free.

**Verdict:** Slight positive for mache on token discipline; built-in returns
strictly more useful context (the actual call line). On a 5K-LOC file this
wouldn't matter; on a 30K-LOC repo, neither dominates.

### 3.6 Subclasses / implementations (Task 5)

Target: implementations of the `Graph` interface (`internal/graph/graph.go:112`).

**Mache:** `find_definition Graph` → `not found`. `search %Graph% role=definition`
→ `[]`. No working path to "what implements this interface?" without falling
back to grep.

**Built-in:** `grep -rn 'GetNode(id string)' --include='*.go' .` → 10 hits
covering nine implementer types (`readOnlyGraph`, `lazyGraph`, `udsGraph`,
`GraphCache`, `SQLiteWriter`, `minimalStore`, `WritableGraph`, `mockGraph`,
`SQLiteGraph`) plus the interface declaration. One shot, complete answer.

**Verdict:** Negative for mache. Interface-implementer discovery requires
either type-aware indexing or LSP `textDocument/implementation`; mache's refs
index is call-site-only, so it cannot answer this class of question.

### 3.7 External-dependency symbol (Task 6)

Target: `sql.Open`.

**Mache:** `get_type_info sql.Open` → error: `no _lsp_hover table`. Hint
points to running `ll-open enrich-lsp` (out-of-band setup).

**Built-in:** `go doc database/sql.Open` → 20-line signature + docstring in
one shell call.

**Verdict:** Built-in wins this projection. Mache is contractually capable
*if* LSP tables are baked in — that requires ley-line preprocessing that this
checkout does not have.

### 3.8 Scope precision by name path (Task 14)

This is the headline mache claim — "symbols addressed by dotted name path."

**Mache:**

- `find_definition MemoryStore.GetCallers` → `not found`.
- `find_definition SQLiteGraph.GetCallers` → `not found`.
- `find_definition GetCallers` (bare) → `not found`.

`GetCallers` has 11 receivers in this repo (verified via grep). Mache offers
no way to disambiguate by receiver — `find_callers GetCallers` aggregates all
of them indistinguishably, and `find_definition` returns nothing.

**Built-in:** `grep -rn 'func.*GetCallers' --include='*.go' .` →
10 lines, receiver visible in each (`(s *MemoryStore)`, `(g *SQLiteGraph)`,
`(lg *lazyGraph)`, etc.). Scope disambiguation is in the result line itself.

**Verdict:** Negative for mache on its own headline claim. The dotted
name-path resolver does not work on this projection.

### 3.9 Multi-step chained exploration (Task 18)

Workflow: starting from `SocketClient.SendOp`, find callers, then for one of
those callers find its callees.

**Mache:**

1. `find_callers SendOp` → 17 construct paths. ✓
1. To pick a caller and look at *its* callees, I need to map a
   construct path back to a method declaration dir. Paths look like
   `internal/leyline/socket.go/method_declaration_4/block/statement_list/.../call_expression`.
   The method-decl prefix is the construct dir, but the name (`Tool`, in
   this case) is only visible after a `field_identifier` read.
1. `find_callees internal/leyline/socket.go/method_declaration_1` (SendOp
   itself) → `{"callees": []}`. SendOp's body invokes `sendRaw`,
   `json.Unmarshal`, `fmt.Errorf` — none returned. Stdlib calls and
   receiver-method calls aren't resolved.

Intermediate addresses *do* persist across calls (construct paths are
stable), but the callee resolver isn't powerful enough to make the chain
worth continuing.

**Built-in:** `grep -rn 'SendOp'` → 17 hits with `file:line`. Pick one,
`Read` 20 lines around it, see the call chain in source.

**Verdict:** Neutral-to-negative. Stable IDs are real, but find_callees'
empty-set failures break the chain that would justify them.

### 3.10 Non-code file read (Task 19) and free-text search (Task 20)

- **Read Taskfile.yml.** `mache read_file Taskfile.yml` → error "is a
  directory" (every file is a projected directory). `Read` works directly.
- **Search `ErrNotFound`.** `find_callers ErrNotFound` → `[]`;
  `search %ErrNotFound%` → `[]`. `grep -rn 'ErrNotFound' --include='*.go'`
  → 85 hits. Variable-name references are not in mache's refs index.

**Verdict:** Both tasks are squarely in built-in territory, and mache's
attempted analogs are not just unhelpful but give misleadingly empty answers.

______________________________________________________________________

## 4. Token-efficiency analysis

| Workflow                                  | Mache calls                                                                           | Mache input/output                  | Built-in calls     | Built-in input/output |
| ----------------------------------------- | ------------------------------------------------------------------------------------- | ----------------------------------- | ------------------ | --------------------- |
| Top-level overview (Task 1)               | 1                                                                                     | 0 / 2.9K tok                        | 3 (ls, find, Read) | 0 / ~1.5K tok         |
| Large-file outline (Task 2)               | 2+ (list + ~40 batch-read field_identifier files)                                     | 0 / ~3K tok                         | 1 grep             | 0 / ~0.5K tok         |
| Method body (Task 3)                      | 5+ (find_def fail → list → field_identifier reads → list block → list statement_list) | 0 / ~1.5K tok then stuck            | 1 Read with offset | 0 / ~0.3K tok         |
| `find_callers NewMemoryStore`             | 1                                                                                     | 0 / ~10K tok                        | 1 grep             | 0 / ~13K tok          |
| Interface implementers (Task 5)           | 2 (find_def fail + search fail)                                                       | 0 / ~0.1K tok then stuck            | 1 grep             | 0 / ~1K tok           |
| `get_architecture`                        | 1                                                                                     | 0 / 720K tok (truncated by harness) | n/a                | n/a                   |
| `get_communities summary=true min_size=5` | 1                                                                                     | 0 / 486K tok (truncated by harness) | n/a                | n/a                   |

**Stable vs ephemeral addressing.** Construct paths are stable across reads
within a session, which would matter for refactors — but mache is read-only
here. Grep results carry line numbers, which are ephemeral but adequate for
read-only inspection.

**Forced reads.** Mache forces extra reads in two patterns: (1) construct dirs
expose no symbol name, so to identify a method by name you must read its
`field_identifier`; (2) `read_file` rejects non-leaf paths, so resolving a
multi-statement method body needs recursive descent. Both are projection
properties, not query-API limitations.

**Payload blow-up.** `get_architecture` and `get_communities` exceed the
agent response budget without `limit`-style parameters that actually trim
output. On a 28K-LOC repo. That alone prevents using these from a single
agent turn.

**Verdict:** Built-in tools are more token-efficient on this corpus across
every task tested except `find_callers` on a moderately-used function, where
the two are roughly even.

______________________________________________________________________

## 5. Bottom-line ordering (frequency × value-per-hit)

Ranked from highest combined daily impact to lowest:

1. **Method/function call-site discovery (`find_callers`, function tokens):**
   *Positive.* High frequency, medium value. Cleaner than grep when you want
   only real call sites, no docs/strings. Works.
1. **Method-body retrieval (`find_definition` + body extract):** *Negative.*
   High frequency, high value-per-hit. `find_definition` is broken on common
   symbols, the projection doesn't yield reassembled bodies, and `Read` with
   an offset wins every time.
1. **Interface implementers / type references (`find_callers` on types):**
   *Negative correctness hazard.* Medium frequency, high value-per-hit (a
   missed implementer is a real bug magnet). Returns silently empty.
1. **File-level structural outline (`list_directory` on a .go file):**
   *Negative.* High frequency, medium value. `grep -nE '^(func|type)…'`
   produces a strictly richer outline with line numbers and signatures.
1. **Free-text / variable-name search:** *Negative*, declared out-of-scope
   but the tool surface still appears to advertise it. Grep wins.
1. **Architectural overview (`get_architecture`, `get_communities`):**
   *Negative for fresh-context exploration.* Payloads are too large to
   consume; top entries are stdlib + testify noise.
1. **External-dependency signatures:** *Out of scope here* (no LSP enrichment
   in this projection). `go doc` works in one bash call.

The one capability mache contributes uniquely on this corpus —
function-call-site listing with refs-index discipline — is real but
matches a single bullet of Serena's full eval rubric. The headline
"navigation by name path" claim does not survive contact with broken
`find_definition` / `role=definition` filters and missing type-reference
capture. Net effect on a fresh-context agent: built-in `Grep` + `Read`
remains the load-bearing pair; `find_callers` is a worthwhile augment when
you already know the function name you care about.

**Practical usage rule.** Use `mcp__mache__find_callers <funcName>` when you
need a clean call-site listing for a known function/method name; fall back to
`Grep` for everything else (file outlines, definitions, type references,
interface implementers, free-text, non-code files). Avoid `get_architecture`
and `get_communities` from inside an agent turn on a repo of this size until
they ship a `top_n`-style cap.

______________________________________________________________________

## Appendix: tasks classified out of scope per Serena's taxonomy

- Tasks 7a/7b/7c (single-file edits at small/medium/large scope) — mache is
  read-only on this projection. No edit tool exposed.
- Tasks 8, 9 (symbolic insert, semantic rename) — same.
- Tasks 10, 11, 12, 13 (cross-file rename/move/delete/inline) — same.
- Tasks 15, 16 (atomicity, success signals for refactors) — no refactor surface
  to exercise.
- Task 17 (chained edits) — no edits.

These are not findings against mache; they are simply outside its current
read-only MCP contract on this checkout.
