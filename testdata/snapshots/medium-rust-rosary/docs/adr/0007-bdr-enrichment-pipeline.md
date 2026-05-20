# ADR-0007: BDR Enrichment Pipeline

**Status:** Proposed
**Author:** Theoretical Foundations Analyst (Opus 4.6)
**Date:** 2026-03-19
**Depends on:** ADR-0004 (Dual State Machine)
**Relates to:** ADR-0006 (Declarative Tool Registry)

## Context

BDR (Bead-Driven Requirements) currently performs naive markdown-to-bead decomposition:
ADR markdown -> atoms (section classification) -> BeadSpecs (title, description, priority).
This produces beads with unresolved references, no verified file scopes, no dedup against
existing work, and no validation of decomposition quality. The result is "garbage in"
for the dispatch pipeline -- beads that overlap with existing work, reference symbols
that don't exist, or scope work to the wrong repository.

The enrichment pipeline closes the gap between textual decomposition and actionable,
verified work items.

## Decision

### A. Enrichment Pipeline Architecture

The pipeline transforms `BeadSpec` (raw) into `EnrichedBeadSpec` through four sequential
stages. Each stage has a well-defined contract: specific inputs, specific outputs, and
a specific fallback when the stage cannot complete.

```
BeadSpec (raw)
    |
    v
[Stage 1: Symbol Resolution]  -- mache MCP
    |
    v
SymbolResolved<BeadSpec>
    |
    v
[Stage 2: Dedup Check]        -- sqlite-vec + epic.rs
    |
    v
DedupChecked<BeadSpec>
    |
    v
[Stage 3: LLM Validation]     -- haiku
    |
    v
Validated<BeadSpec>
    |
    v
[Stage 4: Materialization]    -- write to Dolt
    |
    v
EnrichedBeadSpec (with files, dedup_status, quality_score)
```

#### Stage 1: Symbol Resolution

**Input:** BeadSpec with `references: Vec<String>` (backtick strings, markdown links).

**Process:**

1. For each reference in `references`, classify it:

   - Symbol name (alphanumeric + underscore, no path separators): query `mache search`
   - File path (contains `/` or `.rs`/`.go`/`.py` extension): validate existence via `mache list_directory`
   - Repo-qualified reference (`repo:symbol`): split, validate repo against `rosary.toml`, query mache for the target repo
   - URL: preserve as-is (no resolution needed)

1. For each resolved symbol, expand scope via `mache find_callers` and `mache find_callees` to compute the **blast radius** -- the transitive closure of files that could be affected by changes to that symbol. Limit traversal depth to 2 hops to bound computation.

1. Union all resolved file paths into the BeadSpec's `files` field. Separate test files (matching `*_test.*`, `tests/`, `test_*`) into `test_files`.

**Output:** BeadSpec with populated `files` and `test_files`.

**Fallback:** If mache returns no results for a symbol:

- Mark the reference as `unresolved` in a new `resolution_status` map
- Do NOT populate files from unresolved references
- Continue pipeline -- the bead gets created with partial scope
- Log a warning: unresolved symbols may indicate the bead targets the wrong repo

**Complexity:** O(R * D) where R = number of references, D = average call graph depth. With the 2-hop limit and typical R < 10, this is O(20) mache queries per BeadSpec. For an ADR producing ~10 BeadSpecs, total = ~200 mache queries. At ~50ms per MCP call, that is ~10 seconds per ADR decomposition. Acceptable.

**Correctness guarantee:** File scopes are ONLY populated from mache-verified symbols. No guessing. This is the core safety property -- `has_file_overlap()` in epic.rs depends on accurate scopes. False scopes cause false overlaps (agents blocked unnecessarily) or false non-overlaps (agents collide).

#### Stage 2: Dedup Check

**Input:** SymbolResolved BeadSpec + existing bead corpus (from Dolt scan).

**Process:**

1. **Token similarity** (existing): `combined_similarity()` from epic.rs against all open beads in the target repo. O(n) where n = existing beads. This catches obvious textual duplicates.

1. **Embedding similarity** (new): Compute 384D MiniLM embedding of `title + description`. Query sqlite-vec for the k=5 nearest neighbors among existing bead embeddings. Cosine similarity threshold: 0.85 for auto-dedup, 0.70 for flag-for-review, \<0.70 for allow.

1. **Structural overlap** (new): Compare the resolved `files` set of the candidate against `files` sets of existing open beads using `has_file_overlap()` from epic.rs. This catches beads that describe different work but touch the same code.

1. **Composite score:**

   ```
   dedup_score = max(
       token_similarity,
       embedding_similarity * 0.9,      -- slight discount: embeddings miss structural context
       structural_overlap ? 0.7 : 0.0   -- hard floor: same files = suspicious
   )
   ```

1. **Decision boundary:**

   - `dedup_score >= 0.85`: AUTO_DEDUP. Do not create bead. Reference the existing bead.
   - `0.60 <= dedup_score < 0.85`: FLAG_FOR_REVIEW. Create bead in `backlog` state with a comment linking to the potentially duplicated bead(s).
   - `dedup_score < 0.60`: ALLOW. No dedup concern.

**Output:** BeadSpec annotated with `dedup_status: {AutoDedup | FlagForReview | Allow}` and `similar_beads: Vec<(bead_id, score)>`.

**Key insight on the dedup formalization (Section B):**

Two beads represent "the same work" when ANY of these conditions hold:

1. **Semantic equivalence**: They describe the same change in different words (embedding cosine > 0.85)
1. **Subsumption**: One bead's scope is a proper subset of another's (file set containment + semantic overlap > 0.6)
1. **Structural identity**: They resolve to the same call graph neighborhood (>80% file overlap AND title semantic similarity > 0.5)

Condition 1 is necessary but not sufficient alone -- two beads can be semantically similar but target different repos. Condition 3 is necessary but not sufficient alone -- two beads can touch the same files for different reasons (one adds a function, one refactors an existing one). The composite score captures the conjunction.

What does NOT constitute duplication:

- Same files, different symbols (e.g., "add method X to serve.rs" vs "fix method Y in serve.rs")
- Same description, different repos (cross-repo work is explicitly not a duplicate)
- Sequential phases of the same work ("Phase 1: scaffold" and "Phase 2: wire" share scope but are ordered, not duplicated -- this is already handled by epic.rs `ClusterRelationship::Sequential`)

#### Stage 3: LLM Validation

**Input:** DedupChecked BeadSpec.

**Process:** Call Claude Haiku with a structured prompt:

```
Given this bead specification:
  Title: {title}
  Description: {description}
  Files in scope: {files}
  Source ADR: {source_adr}
  Source atom kind: {source_atom}

And these similar existing beads:
  {similar_beads with titles and statuses}

Evaluate:
1. SCOPE: Is this bead appropriately sized? (1-3 files = good, 4-8 = consider splitting, 8+ = must split)
2. CLARITY: Does the title accurately describe the work? Is it actionable?
3. OVERLAP: Given the similar beads listed, is this genuinely new work?
4. COMPLETENESS: Are the success criteria verifiable?

Respond with JSON:
{
  "quality_score": 0.0-1.0,
  "should_split": boolean,
  "split_suggestion": "how to split, if applicable",
  "title_revision": "improved title, if needed, else null",
  "overlap_verdict": "new_work | partial_overlap | duplicate",
  "notes": "any other concerns"
}
```

**Output:** BeadSpec with `quality_score`, optional `revised_title`, and `split_suggestions`.

**Fallback:** If haiku is unavailable or returns malformed JSON:

- Set `quality_score = 0.5` (neutral)
- Set `validation_status = "skipped"`
- Continue pipeline -- LLM validation is advisory, not gating
- Log the failure for operator review

**Cost:** Haiku at ~$0.25/1M input tokens, ~$1.25/1M output tokens. A typical validation prompt is ~500 tokens input, ~200 tokens output. Per bead: ~$0.0004. Per ADR (10 beads): ~$0.004. Negligible.

**Why haiku and not a larger model:** The validator does not need deep reasoning. It needs pattern matching: "is 12 files too many for one bead?" and "does this title match this description?" These are classification tasks, not generation tasks. Haiku is sufficient and 10-20x cheaper than Sonnet.

#### Stage 4: Materialization

**Input:** Validated BeadSpec.

**Process:**

1. If `dedup_status == AutoDedup`: skip creation, return reference to existing bead.
1. If `quality_score < 0.3` and `should_split == true`: return split suggestions without creating. The caller (rsry_decompose or intake pipeline) decides whether to auto-split or prompt the user.
1. Otherwise: create bead via Dolt with all enriched fields:
   - `files` from Stage 1
   - `test_files` from Stage 1
   - `dedup_status` comment from Stage 2
   - `quality_score` in bead metadata
   - `source_adr` preserved from BeadSpec
   - `depends_on` from ADR frontmatter + Stage 1 cross-repo resolution

**Output:** Created bead ID or enrichment report (for dedup/split cases).

### B. Dedup Formalization

The dedup problem is a classification problem over pairs of beads. Given candidate bead `c`
and existing bead `e`, classify the pair into one of:

- **DUPLICATE**: `c` and `e` describe the same work. Do not create `c`.
- **SUBSUMES**: `e` already covers the work in `c`. Do not create `c`.
- **RELATED**: `c` and `e` are related but distinct. Create `c` with a link to `e`.
- **INDEPENDENT**: `c` and `e` are unrelated. Create `c`.

**Feature vector for the classifier:**

| Feature              | Source                                                    | Range  |
| -------------------- | --------------------------------------------------------- | ------ |
| `title_jaccard`      | epic.rs `jaccard_filtered`                                | [0, 1] |
| `desc_jaccard`       | epic.rs `jaccard_similarity`                              | [0, 1] |
| `embedding_cosine`   | sqlite-vec on MiniLM 384D                                 | [0, 1] |
| `file_overlap_ratio` | \|files_c intersect files_e\| / \|files_c union files_e\| | [0, 1] |
| `scope_containment`  | files_c subset files_e (boolean)                          | {0, 1} |
| `same_repo`          | c.repo == e.repo                                          | {0, 1} |
| `sequential_pattern` | epic.rs `sequential_similarity`                           | [0, 1] |
| `scope_prefix`       | epic.rs `scope_prefix_similarity`                         | [0, 1] |

**Decision boundaries (hand-tuned, to be validated empirically):**

```
DUPLICATE:    embedding_cosine > 0.85 AND same_repo
SUBSUMES:     scope_containment AND embedding_cosine > 0.60 AND same_repo
RELATED:      file_overlap_ratio > 0.3 OR embedding_cosine > 0.60
INDEPENDENT:  otherwise
```

**Why not a learned classifier:** The training data does not exist yet. Agent-created beads
at scale are new. Start with hand-tuned thresholds, collect (candidate, existing, human_verdict)
triples during the flag-for-review period, then train a lightweight classifier when N > 100
examples exist. The feature vector is designed to be classifier-ready.

**Theoretical note on metric space composition:**

Jaccard distance is a metric on the power set 2^V (token sets). Cosine distance on R^384
is a pseudo-metric (not a true metric because cos(a,b) = 1 does not imply a = b -- it implies
collinearity). File overlap ratio is a Jaccard-like pseudo-metric on file sets.

These three distances live in different metric spaces. The composite score
`max(token, embedding * 0.9, structural)` is NOT a metric -- it violates the triangle
inequality. This is acceptable because we are not performing nearest-neighbor search in
the composite space; we use it only for threshold classification. If we later need proper
metric properties (for clustering), we should use a weighted sum rather than max, and
verify triangle inequality holds for the chosen weights.

### C. Cross-Repo Routing

#### The Problem

An ADR like "Sheaf Cache" produces beads for ley-line (cache implementation), mache
(structural indexing), and rosary (pipeline integration). Each bead must be created in
the correct repo's Dolt database and scoped to files in that repo.

#### Routing Algorithm

1. **Explicit routing** (highest priority): If the ADR frontmatter contains `repo: leyline`,
   all beads default to ley-line. If an atom's references contain `mache:find_definition`,
   that specific bead routes to mache. This is `infer_target_repo()` today, unchanged.

1. **Symbol-based routing** (Stage 1 output): After symbol resolution, if a bead's resolved
   files all reside in a single repo, route to that repo. If files span repos, flag for
   manual routing.

1. **Fallback**: If no routing signal exists, route to the ADR's source repo (where the
   ADR markdown file lives).

#### Cross-Repo Dependencies

When bead A (in rosary) depends on bead B (in mache):

1. Create bead A with `external_ref: "mache:bead-B-id"` (existing field on Bead struct)
1. Create a dependency record in rosary's orchestrator store (`store_dolt.rs` dependencies table)
1. The reconciler already checks `dependency_count > 0` before dispatch

The `external_ref` format `"repo_name:bead_id"` maps to repo paths via `rosary.toml`.

#### Multi-Repo Mache Indexing

Mache currently projects one codebase at a time. When enriching beads across repos:

**Option A: Sequential projection switches.** Before enriching beads for repo X, ensure
mache is projecting repo X. This is slow (projection rebuild) but correct.

**Option B: Multiple mache instances.** Run one mache process per repo. The enrichment
pipeline queries the right instance based on the bead's target repo. This is the
recommended approach -- mache instances are lightweight (SQLite-backed) and can run
concurrently.

**Option C: mache query routing (future).** A single mache instance that maintains
multiple projections and routes queries by repo context. This requires mache changes
and is deferred.

**Recommendation: Option B.** The enrichment pipeline maintains a `HashMap<RepoName, MacheMcpClient>` mapping repo names to mache MCP endpoints. When resolving symbols for a bead
targeting repo X, query `mache_clients["X"]`. If no mache instance exists for repo X,
fall back to unresolved references (Stage 1 fallback).

### D. Dependency Boundary: rosary vs ley-line

**Question:** Does rosary need a direct Rust dependency on ley-line for sqlite-vec
embedding support?

**Analysis:**

Current state:

- rosary depends on `leyline-vcs` (Git operations only, from ley-line's public VCS crate)
- ley-line is closed source (proprietary)
- rosary is AGPL-3.0
- `leyline-embed` (MiniLM embeddings) is a ley-line crate, closed source
- sqlite-vec is an open-source SQLite extension

Licensing constraint: AGPL rosary cannot statically link closed-source leyline-embed
without ley-line also being AGPL-compatible. `leyline-vcs` appears to be a separate
public crate, which is fine. But `leyline-embed` is closed.

**Three options:**

1. **Through mache MCP** (recommended for now): mache already has ley-line integration
   (ley-line inserts structural data into mache's SQLite). Add an MCP tool
   `mache_embedding_similarity(text, k)` that computes embeddings via leyline-embed
   and returns similar items from sqlite-vec. Rosary queries this over MCP.

   - Pro: No licensing issue. Clean abstraction boundary.
   - Pro: mache already has the sqlite-vec database populated.
   - Con: MCP latency (~50ms per call). For k=5 neighbors on 10 beads = 500ms total. Acceptable.
   - Con: Requires mache to expose a new tool.

1. **Open-source embedding crate** (future option): Use an open-source Rust embedding
   library (e.g., `rust-bert` or `candle` with a MiniLM ONNX model) directly in rosary.
   Run sqlite-vec as an in-process SQLite extension.

   - Pro: No external dependency at runtime. No MCP latency.
   - Pro: No licensing conflict (open-source model + open-source extension).
   - Con: Large binary size (ONNX runtime). Model download on first run.
   - Con: Duplicates functionality already in ley-line.

1. **ley-line as a service** (future option): ley-line exposes embedding computation
   over its own MCP or HTTP interface. Rosary calls it as a service.

   - Pro: Clean separation. ley-line owns embeddings.
   - Con: Another service to run. Operational complexity.

**Recommendation:** Start with Option 1 (mache MCP). If latency becomes a bottleneck
or mache is not always available, migrate to Option 2 with an open-source embedding
model. Do not take a direct dependency on leyline-embed from rosary.

### E. Pre-flight Check Protocol

Before the enrichment pipeline runs, these invariants must hold:

#### Required (pipeline will not start without these)

1. **Dolt server reachable**: The target repo's Dolt database must be accessible.
   Check: `SELECT 1` on the connection. Failure mode: abort with clear error message.

1. **ADR parseable**: `parse_adr_full()` must return at least one atom.
   Check: `atoms.len() > 0`. Failure mode: return empty enrichment result.

1. **Target repo in rosary.toml**: The ADR's target repo(s) must be registered.
   Check: all repos referenced in frontmatter or atom references exist in config.
   Failure mode: warn and continue with unresolvable references.

#### Optional (pipeline degrades gracefully without these)

4. **mache available**: At least one mache MCP instance is reachable.
   Check: `mache get_overview` returns successfully.
   Degraded mode: skip Stage 1 (symbol resolution). Beads created without file scopes.
   Impact: no file overlap detection, no blast radius computation.

1. **Embedding index populated**: sqlite-vec contains embeddings for existing beads.
   Check: `SELECT count(*) FROM bead_embeddings` > 0.
   Degraded mode: skip embedding similarity in Stage 2. Use token-only dedup.
   Impact: miss paraphrase duplicates.

1. **Haiku API key set**: `ANTHROPIC_API_KEY` environment variable is set.
   Check: env var exists and is non-empty.
   Degraded mode: skip Stage 3 (LLM validation). All beads get quality_score = 0.5.
   Impact: no split recommendations, no title improvements.

#### Pre-flight Report

The pre-flight check produces a structured report:

```rust
pub struct PreflightReport {
    pub dolt_ok: bool,
    pub adr_atom_count: usize,
    pub repos_resolved: Vec<String>,
    pub repos_unresolved: Vec<String>,
    pub mache_available: bool,
    pub mache_repos: Vec<String>,   // repos with active mache projection
    pub embeddings_populated: bool,
    pub embedding_count: usize,
    pub haiku_available: bool,
    pub degraded_stages: Vec<String>,
}
```

The caller (rsry_decompose MCP tool or CLI) inspects this report and decides
whether to proceed with degraded enrichment or abort.

## Implementation Plan

### Phase 1: Type-state enrichment framework (rosary, crates/bdr/)

Add the `EnrichedBeadSpec` type and the pipeline skeleton with stage trait:

```rust
pub trait EnrichmentStage {
    type Input;
    type Output;
    async fn enrich(&self, input: Self::Input) -> Result<Self::Output>;
    fn name(&self) -> &str;
    fn is_required(&self) -> bool;
}
```

Files: `crates/bdr/src/enrich.rs` (new), `crates/bdr/src/lib.rs`

### Phase 2: Symbol resolution via mache MCP (rosary)

Implement Stage 1. Requires MCP client for mache (rosary already has MCP infrastructure
from agent-client-protocol). Wire `mache search` and `mache find_callers/find_callees`
into the symbol resolver.

Files: `crates/bdr/src/enrich/resolve.rs` (new), `src/serve.rs` (wire MCP client)

### Phase 3: Embedding-based dedup (mache + rosary)

Add `mache_embedding_similarity` MCP tool to mache. In rosary, extend epic.rs
`combined_similarity()` to incorporate the embedding signal. Add the composite
dedup scorer.

Files: `src/epic.rs`, `crates/bdr/src/enrich/dedup.rs` (new)
Mache files: new MCP tool implementation (separate repo)

### Phase 4: Haiku validation (rosary)

Implement Stage 3 with structured prompt and JSON response parsing.
Add retry logic and graceful fallback.

Files: `crates/bdr/src/enrich/validate.rs` (new)

### Phase 5: Pre-flight + materialization (rosary)

Implement pre-flight checks and Stage 4 materialization. Wire the complete
pipeline into `rsry_decompose` MCP tool and `rsry decompose` CLI.

Files: `crates/bdr/src/enrich/preflight.rs` (new), `src/serve.rs`, `src/main.rs`

## Validation

### Success Criteria

1. An ADR decomposition produces beads with verified file scopes (>80% of references resolved to files)
1. Duplicate beads are caught before creation (recall > 0.9 for beads with embedding cosine > 0.85)
1. Cross-repo beads are routed to the correct repo (100% accuracy for explicit frontmatter routing, >90% for symbol-based routing)
1. The full pipeline completes in \<30 seconds for a typical ADR (10 atoms, 3 repos)
1. Graceful degradation: pipeline produces valid (if less enriched) beads when mache or haiku is unavailable

### What to Measure

- Reference resolution rate: `|resolved_refs| / |total_refs|`
- Dedup precision: `|true_duplicates_caught| / |beads_flagged_as_duplicate|`
- Dedup recall: `|true_duplicates_caught| / |actual_duplicates|`
- Routing accuracy: `|correctly_routed| / |total_cross_repo_beads|`
- Pipeline latency: p50, p95, p99 per ADR decomposition
- Haiku validation agreement rate: how often does haiku agree with the pipeline's dedup decision?

## Consequences

### Positive

- Beads get verified file scopes, enabling accurate overlap detection for parallel dispatch
- Semantic duplicates caught before creation, reducing bead noise
- Cross-repo ADRs produce correctly routed beads
- LLM validation catches oversized beads before dispatch wastes tokens
- Graceful degradation means the pipeline works even without all services

### Negative

- Pipeline adds ~10-30 seconds to decomposition (currently instant)
- Requires mache MCP availability for full enrichment
- New MCP tool needed in mache (embedding similarity)
- Embedding index must be maintained (populated on bead creation, updated on bead close)
- Haiku API costs (negligible but non-zero)

## Open Questions

1. Should the enrichment pipeline run synchronously during `rsry_decompose` or asynchronously
   as a background job that enriches beads after creation?
1. How should we handle beads that resolve to files in a repo not yet indexed by mache?
1. Should the embedding model be configurable (MiniLM vs other models)?
1. At what bead count does O(n) dedup become a bottleneck? (Likely >1000 per repo.
   sqlite-vec ANN handles this but token-based Jaccard does not.)
1. Should split suggestions from haiku be auto-applied or always require human review?

## References

- epic.rs: existing dedup engine (Jaccard + Union-Find clustering)
- crates/bdr/: current BDR decomposition pipeline
- mache MCP tools: find_definition, find_callers, find_callees, search, get_communities
- leyline-embed: MiniLM 384D embedding (closed source, accessed via mache)
- sqlite-vec: vector similarity search in SQLite
- ADR-0004: Dual state machine (bead lifecycle + pipeline phases)
