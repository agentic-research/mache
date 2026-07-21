# QuotientGraph: System Diagrams from Graph Math

**Date**: 2026-03-22
**Status**: Design
**Beads**: mache-ml2, mache-8eb3e6, mache-c0n, mache-tkr, mache-6bb5e7

## Problem

Mache has the structural graph data (refs, defs, communities, call graphs) but no way to render it as a diagram. Hand-written mermaid in README.md and ARCHITECTURE.md goes stale. Ten open beads across mache and rosary all need the same thing: a way to turn graph data into legible mermaid text.

Cross-language connections (Terraform → Go, service → service) exist implicitly in address-like strings (file paths, URLs, env var names) but mache's ref extraction doesn't capture them. The refs system works — it just needs smarter token emission.

## Design Principles

1. **No magic numbers or hardcoded thresholds.** Grouping, labeling, and layout emerge from the graph structure. Community detection decides groups. Token frequency decides labels. Edge weight decides what's shown.
1. **Schema-driven.** Diagrams are defined in schema JSON and rendered via template functions, the same way everything else in mache works.
1. **Address-agnostic.** File paths, URLs, env vars, ARNs — all are addresses pointing to something. The ref system treats them uniformly with typed token prefixes.
1. **Mermaid is the abstraction.** No virtual directories for diagrams. The mermaid text IS the human-legible view. It lives wherever the schema puts it — README, ARCHITECTURE.md, bead descriptions.

## Architecture

### Part 1: QuotientGraph — The Core Type

`internal/graph/quotient.go`

The quotient graph compresses a full node graph into a diagram-scale object by collapsing nodes into equivalence classes (communities) and aggregating inter-class edges.

```go
type QuotientGraph struct {
    Classes    []Class
    Edges      []QuotientEdge
    ClassOf    map[string]int    // node ID → class index
}

type Class struct {
    ID        int
    Label     string      // emergent: most-referenced token among members
    Members   []string    // constituent node IDs
    InternalW float64     // total internal edge weight (density measure)
}

type QuotientEdge struct {
    From         int
    To           int
    Weight       float64                // raw/unnormalized (unlike sheaf.go's sigmoid)
    Tokens       []string               // boundary tokens creating this edge
    TokenWeights map[string]float64     // per-token contribution to Weight
}
```

**`ComputeQuotient(cr *CommunityResult, refs map[string][]string) *QuotientGraph`**

Reuses the pattern from `buildRestrictions()` in `internal/leyline/sheaf.go`:

1. For each ref token, find which communities reference it.
1. Tokens referenced by nodes in multiple communities create inter-class edges.
1. Edge weight = product of member counts referencing the shared token (same formula as `buildRestrictions`, but raw/unnormalized — sheaf.go applies a sigmoid normalization for co-change rates, which is not needed here).
1. Class label = the token with the highest total ref count among class members. Ties broken lexicographically for stability.
1. Internal weight = sum of edge weights between nodes within the same class.

**`Mermaid(layout string) string`**

Renders the quotient graph as mermaid syntax:

- Each `Class` → `subgraph` with the emergent label
- Each `QuotientEdge` → arrow between subgraphs
- Edge annotations use per-token weights to show prominent boundary tokens (those above the mean weight for that edge — an emergent threshold, not hardcoded)
- `layout` maps to mermaid direction: `"TD"`, `"LR"`, `"BT"`, `"RL"`
- Classes with a single member render as a node, not a subgraph
- Empty classes are omitted

**Relationship to existing code:**

- `DetectCommunities()` in `community.go` produces the `CommunityResult` input
- `buildRestrictions()` in `sheaf.go` computes the same cross-community edges but ships them to ley-line. QuotientGraph keeps them as a first-class renderable object.
- `refsMap()` on the `Graph` interface provides the refs input

### Part 2: Address-Aware Ref Extraction

`internal/ingest/engine_languages.go` (extend existing)

Currently, HCL ref queries only capture `source` and `default` string literals. Go only captures qualified calls. This misses the primary cross-language connection: address-like strings.

**Token format:** `<scheme>:<value>`

| Source Pattern                             | Scheme  | Token Example                                   |
| ------------------------------------------ | ------- | ----------------------------------------------- |
| Relative/absolute paths (`./`, `../`, `/`) | `path`  | `path:code/thing`                               |
| URLs (`http://`, `https://`, `s3://`)      | `url`   | `url:http://api:8080`                           |
| Go `os.Getenv("X")`                        | `env`   | `env:DATABASE_URL`                              |
| HCL `variable "X"`                         | `env`   | `env:DATABASE_URL`                              |
| YAML env refs (`${VAR}`, `$VAR`)           | `env`   | `env:DATABASE_URL`                              |
| ARN patterns (`arn:aws:`)                  | `arn`   | `arn:aws:lambda:us-east-1:123:function:handler` |
| Container images (`registry/name:tag`)     | `image` | `image:registry.io/org/api:latest`              |

**Path resolution:** Relative paths resolved against the source file's directory to produce canonical tokens. `../../code/thing` from `env/dev/stage/main.tf` → `path:code/thing`.

**Implementation approach — per-language tree-sitter queries:**

For HCL/Terraform:

```
;; All string literals in attribute values (addresses emerge from content)
(attribute (identifier) @_key (expression (literal_value (string_lit) @ref)))
```

The engine post-processes captured strings: classify by pattern (path/url/env/arn/image), resolve relative paths, emit as typed ref tokens. No per-attribute hardcoding — any string that matches an address pattern gets captured.

For Go:

```
;; os.Getenv calls
(call_expression
  function: (selector_expression
    operand: (identifier) @_pkg
    field: (field_identifier) @_func)
  arguments: (argument_list (interpreted_string_literal) @ref)
  (#eq? @_pkg "os") (#eq? @_func "Getenv"))
```

For YAML:

```
;; String values that look like addresses
(block_mapping_pair value: (flow_node (plain_scalar (string_scalar) @ref)))
```

Post-processing classifies captured strings the same way across all languages.

**Env var bridge:** When Go code has `os.Getenv("DATABASE_URL")` and HCL has `variable "DATABASE_URL" {}`, both emit `env:DATABASE_URL`. The refs store connects them automatically. No special bridge code needed.

### Part 3: Diagram Definitions in Schema

Two new optional top-level keys in schema JSON:

```json
{
  "version": "v1",
  "diagrams": {
    "architecture": { "layout": "TD" },
    "data-flow": { "layout": "LR" }
  },
  "nodes": [ ... ]
}
```

**`diagrams`** — Named diagram views. Each entry is a named view with:

- `layout` (required): mermaid direction (`"TD"`, `"LR"`, `"BT"`, `"RL"`)

No `group_by`, no `filter`, no `depth`. Grouping emerges from community detection on the refs graph. Labels emerge from token frequency. What's shown emerges from edge weight. The diagram definition is intentionally minimal.

Future extensions (not in this spec): `focus` (center on a specific community), `exclude` (hide noise communities), `depth` (hierarchical refinement via `Refine()`).

### Part 4: `{{diagram}}` Template Function

**Wiring challenge:** `RenderTemplate()` in `engine.go` is stateless — the global `tmplFuncs` map has no access to the Engine, Graph, or schema. The `diagram` function needs a closure over the graph's refs and the schema's diagram definitions.

**Approach:** The Engine injects a `diagram` closure into the per-render `FuncMap` (not the global `tmplFuncs`). This requires a variant of `RenderTemplate` that accepts extra func map entries, or the Engine passes the diagram func as an additional template function when rendering content templates. This follows the pattern of how `doc` is injected into match values.

```go
// Usage in content templates:
// {{diagram "architecture"}}
// {{diagram "system"}}
```

When invoked:

1. Look up the diagram definition by name from the schema's `diagrams` map
1. Get the cached `CommunityResult` and `refs` from the graph (see caching note below)
1. Call `ComputeQuotient(communities, refs)`
1. Call `quotient.Mermaid(layout)`
1. Return the mermaid text

If no diagram name matches, return an error comment in the output (`%% diagram "foo" not defined`).

**Schema-level default:** If no `diagrams` key exists in the schema, `{{diagram "system"}}` renders the full quotient with `"TD"` layout as a fallback.

**Scope:** `{{diagram}}` always renders the **full graph's** quotient, regardless of which record's template is being rendered. Community detection operates on the entire refs graph, not per-record subsets.

**Caching:** `DetectCommunities` is O(n\*k) per Louvain iteration. The community result must be computed once per ingestion and cached on the Engine or Graph, not recomputed per template render. The `{{diagram}}` closure captures the cached result.

### Part 5: `get_diagram` MCP Tool

New tool in `cmd/serve.go`:

```json
{
  "name": "get_diagram",
  "description": "Render a mermaid diagram of the projected system's structure",
  "inputSchema": {
    "type": "object",
    "properties": {
      "name": {
        "type": "string",
        "description": "Diagram name from schema (default: full system view)"
      },
      "layout": {
        "type": "string",
        "enum": ["TD", "LR", "BT", "RL"],
        "description": "Override layout direction (default: from diagram definition or TD)"
      }
    }
  }
}
```

Implementation:

1. If `name` provided, look up in schema's `diagrams` map
1. If not, render full system: `DetectCommunities(refs)` → `ComputeQuotient` → `Mermaid`
1. Return mermaid text as string

## Bead Resolution

This design resolves or advances the following beads:

| Bead                                               | How                                                            |
| -------------------------------------------------- | -------------------------------------------------------------- |
| **mache-ml2** (mermaid output)                     | `QuotientGraph.Mermaid()` — direct implementation              |
| **mache-8eb3e6** (BDR hierarchy + mermaid deps)    | `{{diagram "deps"}}` in BDR schema replaces hardcoded template |
| **mache-6bb5e7** (auto-detect HCL/YAML projection) | Address-aware ref extraction captures HCL semantics            |
| **mache-c0n** (external type system projection)    | Address tokens from provider schemas bridge HCL → code         |
| **mache-tkr** (terraform ACI)                      | `get_diagram` renders infrastructure service graph             |
| **rosary-c5266a** (pipeline as DAG)                | Unblocked — mache-ml2 is the prerequisite                      |
| **rosary-1ba4ee** (doc sync)                       | `{{diagram "architecture"}}` in doc templates                  |
| **rosary-82fc59** (README as projection)           | `{{diagram "system"}}` in README schema template               |

## Implementation Order

1. **QuotientGraph type + tests** (`internal/graph/quotient.go`) — DONE
1. **`get_diagram` MCP tool** (`cmd/serve_diagram.go`) — DONE
1. **Schema `diagrams` field** (`api/schema.go`) — add optional field to Topology type (needed by step 4)
1. **`{{diagram}}` template function** (`internal/ingest/engine.go`) — schema-driven rendering with cached communities
1. **Address-aware ref extraction** — SEPARATE SPEC (see below)

Steps 1-2 are independently useful now. Steps 3-4 depend on each other (template func needs schema field). Step 5 is substantially independent (~150-200 LOC, its own bead) and should be prototyped separately — the YAML query in particular needs validation to avoid noisy token emission.

**Note on orphan nodes:** `DetectCommunities` filters communities by `minCommunitySize`. Nodes in small communities are absent from `CommunityResult.Membership` and are implicitly excluded from the quotient graph. This is the desired behavior — small communities are noise.

## Non-Goals

- **Virtual directories for diagrams.** Mermaid text is the abstraction; filesystem projection adds nothing.
- **Bidirectional diagram editing.** Diagram → code write-back is a future concern. This spec is read-only.
- **Custom grouping config.** No `group_by`, `filter`, or manual group definitions. Everything emerges from the graph.
- **Edge provenance.** Declared/verified/statistical confidence levels are important but deferred. The refs system works without them.
- **Tropo integration.** Sheaf validation of the quotient graph is a natural extension but not in scope.

## Testing Strategy

- **QuotientGraph unit tests:** synthetic graphs with known community structure, verify mermaid output
- **Address extraction tests:** per-language string classification (path/url/env/arn/image)
- **Integration test:** mount a mixed Go+HCL project, verify cross-language refs connect, render diagram
- **Self-hosting test:** `task test-go-schema` already ingests mache's own source — extend to verify `get_diagram` produces valid mermaid
