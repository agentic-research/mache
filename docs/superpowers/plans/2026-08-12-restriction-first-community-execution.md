# Restriction-First Community Execution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute community analysis over exact projected subgraphs and reuse
identical derived results, with deterministic and profiled proof that irrelevant
repository content no longer drives projection cost.

**Architecture:** Add a canonical `RefRestriction`/`RefSnapshot` boundary to the
public graph capabilities, push graph-root predicates into SQLite, and preserve
the semantics across memory, composite, and lazy backends. Route community
consumers through a per-graph singleflight cache keyed by restriction and
algorithm options; keep cached refs/results immutable and expose deterministic
row/pair work counters.

**Tech Stack:** Go 1.25, modernc SQLite, BLAKE3, `x/sync/singleflight`, MCP Go,
ley-line-open v0.18.2, Go pprof, and Brendan Gregg FlameGraph.

**Spec:**
`docs/superpowers/specs/2026-08-12-restriction-first-community-execution-design.md`

## Global Constraints

- Keep the parser/build path pure Go/no CGO and keep LLO pinned to v0.18.2.
- The first selector is an induced subgraph over projected graph-node roots.
  Source paths and import/reference closure are separate follow-ups.
- Reject an explicitly supplied empty `roots`; absent `roots` means whole graph.
- Match an exact root or its `/` descendants. `pkg/a` never selects `pkg/ab`.
- Never add unselected boundary nodes implicitly.
- Gate CI on semantic equivalence and deterministic work, never elapsed time.
- Profiles/flamegraphs are required evidence, not correctness gates.
- Cached snapshots/results are immutable; copy before sorting or truncating.
- LLO already has `idx_refs_node`; add it to Mache's standalone writer.

______________________________________________________________________

### Task 1: Canonical Restriction and Snapshot Values

**Files:**

- Create: `graph/ref_restriction.go`
- Create: `graph/ref_restriction_test.go`
- Modify: `graph/capabilities.go:59-65`

**Interfaces:**

- Produces `RefRestriction`, `NormalizedRefRestriction`, `RefSnapshot`,
  `NormalizeRefRestriction`, `SnapshotRestrictedRefs`, `SnapshotAllRefs`, and
  `RestrictedRefsMapper`.

- `RefSnapshot.Refs` is read-only after construction.

- [ ] **Step 1: Write failing canonicalization and digest tests**

```go
func TestNormalizeRefRestriction_CanonicalRootsAndBoundaries(t *testing.T) {
	r, err := NormalizeRefRestriction(RefRestriction{Roots: []string{
		"/pkg/a/child", "pkg/a", "pkg/ab", "pkg/a", "./pkg/c/",
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/a", "pkg/ab", "pkg/c"}, r.Roots)
	assert.True(t, r.Contains("pkg/a/function/F"))
	assert.False(t, r.Contains("pkg/abc"))
}

func TestNormalizeRefRestriction_RejectsEmptyAndEscapingRoots(t *testing.T) {
	for _, roots := range [][]string{nil, {}, {""}, {"."}, {"../pkg"}, {"pkg/../other"}} {
		_, err := NormalizeRefRestriction(RefRestriction{Roots: roots})
		require.Error(t, err)
	}
}

func TestRefSnapshot_DigestsAndCountersAreOrderIndependent(t *testing.T) {
	r, err := NormalizeRefRestriction(RefRestriction{Roots: []string{"target"}})
	require.NoError(t, err)
	a := SnapshotRestrictedRefs(map[string][]string{
		"Shared": {"target/B", "noise/X", "target/A"}, "Only": {"target/A"},
	}, r)
	b := SnapshotRestrictedRefs(map[string][]string{
		"Only": {"target/A"}, "Shared": {"target/A", "target/B", "noise/X"},
	}, r)
	assert.Equal(t, a.RestrictionDigest, b.RestrictionDigest)
	assert.Equal(t, a.ContentDigest, b.ContentDigest)
	assert.Equal(t, uint64(3), a.RowsRead)
	assert.Equal(t, uint64(2), a.Tokens)
	assert.Equal(t, uint64(2), a.Nodes)
}
```

- [ ] **Step 2: Verify the tests fail for missing API**

```bash
go test ./graph -run 'TestNormalizeRefRestriction|TestRefSnapshot' -count=1
```

Expected: compile failure for the missing restriction/snapshot types.

- [ ] **Step 3: Implement canonicalization and BLAKE3 addresses**

```go
const refRestrictionScheme = "mache-ref-restriction/v1"

type RefRestriction struct { Roots []string `json:"roots"` }
type NormalizedRefRestriction struct {
	Roots []string `json:"roots"`
	Digest string `json:"restriction_digest"`
}
type RefSnapshot struct {
	Refs map[string][]string `json:"-"`
	Roots []string `json:"roots,omitempty"`
	RestrictionDigest string `json:"restriction_digest"`
	ContentDigest string `json:"content_digest"`
	RowsRead uint64 `json:"rows_read"`
	Tokens uint64 `json:"tokens"`
	Nodes uint64 `json:"nodes"`
}
```

Normalize with `path.Clean`, reject empty/current/escaping roots, sort/dedupe,
and remove descendants of selected ancestors. Hash a version tag plus
length-prefixed strings; never use delimiter concatenation. Snapshot builders
sort copied tokens/node slices without mutating callers, preserve duplicate
selected rows, and use a versioned whole-graph sentinel for `SnapshotAllRefs`.

- [ ] **Step 4: Export the capability**

```go
type RestrictedRefsMapper interface {
	RefsWithin(context.Context, RefRestriction) (*RefSnapshot, error)
}
```

- [ ] **Step 5: Run tests and commit**

```bash
go test ./graph -run 'TestNormalizeRefRestriction|TestRefSnapshot' -count=1
go test ./graph -count=1
git add graph/ref_restriction.go graph/ref_restriction_test.go graph/capabilities.go
git commit -m '[mache-b37cff] feat(graph): address reference restrictions'
```

______________________________________________________________________

### Task 2: Backend Pushdown and Parity

**Files:**

- Create: `graph/restricted_refs_test.go`
- Modify: `graph/memstore_refs.go`
- Modify: `graph/sqlite_graph_refs.go:386-451`
- Modify: `graph/composite.go`
- Modify: `cmd/serve_registry.go:1037-1050`
- Modify: `internal/ingest/sqlite_writer.go:84-96`
- Modify: `internal/fixturedb/schema_standalone.go:41-63`

**Interfaces:**

- Consumes Task 1 snapshot constructors.

- Produces `RefsWithin` on MemoryStore, SQLiteGraph, CompositeGraph, lazyGraph;
  produces `CompositeGraph.RefsMap`; produces standalone `idx_refs_node`.

- [ ] **Step 1: Write failing producer/backend parity tests**

```go
func TestRefsWithin_BackendParityAndPathBoundary(t *testing.T) {
	for _, tc := range restrictedBackendFixtures(t) {
		t.Run(tc.name, func(t *testing.T) {
			g, cleanup := tc.open(t)
			defer cleanup()
			rp, ok := g.(RestrictedRefsMapper)
			require.True(t, ok)
			snap, err := rp.RefsWithin(context.Background(), RefRestriction{Roots: []string{"pkg/a"}})
			require.NoError(t, err)
			require.NotEmpty(t, snap.Refs["Shared"])
			for _, ids := range snap.Refs {
				for _, id := range ids {
					assert.True(t, id == "pkg/a" || strings.HasPrefix(id, "pkg/a/"), id)
					assert.False(t, strings.HasPrefix(id, "pkg/ab"), id)
				}
			}
		})
	}
}
```

Build fixtures with `internal/fixturedb` for Standalone and Leyline plus a
MemoryStore; do not handwrite DDL. Seed `pkg/a/F`, `pkg/a/G`, `pkg/ab/H`, and
`noise/X` with `Shared`; Leyline call-site IDs live beneath their containers.

- [ ] **Step 2: Write the failing index-plan test**

```go
func TestSQLiteRefsWithin_UsesNodeIDIndex(t *testing.T) {
	for _, open := range []func(*testing.T) (Graph, func()){
		openRestrictedStandaloneFixture, openRestrictedLeylineFixture,
	} {
		g, cleanup := open(t)
		rows, err := g.(RefsQuerier).QueryRefs(`EXPLAIN QUERY PLAN
			SELECT token, node_id FROM node_refs
			WHERE node_id = ? OR (node_id >= ? AND node_id < ?)`,
			"pkg/a", "pkg/a/", "pkg/a0")
		require.NoError(t, err)
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		require.NoError(t, rows.Close())
		cleanup()
		assert.Contains(t, strings.Join(details, "\n"), "idx_refs_node")
	}
}
```

- [ ] **Step 3: Add the standalone index and derived fixture contract**

```sql
CREATE INDEX IF NOT EXISTS idx_refs_node ON node_refs(node_id);
```

```go
"idx_refs_node": `CREATE INDEX idx_refs_node ON node_refs(node_id)`,
```

- [ ] **Step 4: Implement memory filtering and SQLite pushdown**

```go
func (s *MemoryStore) RefsWithin(ctx context.Context, r RefRestriction) (*RefSnapshot, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	n, err := NormalizeRefRestriction(r)
	if err != nil { return nil, err }
	return SnapshotRestrictedRefs(s.RefsMap(), n), nil
}
```

For nodes-table SQLite, build one parameterized `UNION ALL` arm per normalized
root and append `ORDER BY token, node_id`:

```sql
SELECT token, node_id FROM node_refs
WHERE node_id = ? OR (node_id >= ? AND node_id < ?)
```

Bind `root`, `root + "/"`, `root + "0"`; use `QueryContext`; surface query,
scan, row, and cancellation errors. For legacy bitmap DBs, filter the decoded
map honestly without claiming SQL pushdown.

- [ ] **Step 5: Add composite and lazy routing**

```go
func TestCompositeRefsWithin_RoutesMountRoots(t *testing.T) {
	left, right := NewMemoryStore(), NewMemoryStore()
	require.NoError(t, left.AddRef("Shared", "pkg/A"))
	require.NoError(t, right.AddRef("Shared", "pkg/B"))
	c := NewCompositeGraph()
	require.NoError(t, c.Mount("left", left))
	require.NoError(t, c.Mount("right", right))
	snap, err := c.RefsWithin(context.Background(), RefRestriction{Roots: []string{"left/pkg"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"left/pkg/A"}, snap.Refs["Shared"])
	assert.Equal(t, []string{"left/pkg/A", "right/pkg/B"}, c.RefsMap()["Shared"])
}
```

Group outer roots by mount, strip the mount for delegation, treat a mount root
as that mount's whole `RefsMap`, and re-prefix returned IDs. LazyGraph delegates
after `get()` like its current `RefsMap`.

- [ ] **Step 6: Verify and commit**

```bash
go test ./internal/fixturedb -run TestStandaloneSchema_MatchesSQLiteWriter -count=1
go test ./graph -run 'TestRefsWithin|TestSQLiteRefsWithin|TestCompositeRefsWithin' -count=1
go test -race ./graph -run 'TestRefsWithin|TestCompositeRefsWithin' -count=1
git add graph/restricted_refs_test.go graph/memstore_refs.go graph/sqlite_graph_refs.go graph/composite.go cmd/serve_registry.go internal/ingest/sqlite_writer.go internal/fixturedb/schema_standalone.go
git commit -m '[mache-b37cff] feat(graph): push restrictions into reference stores'
```

______________________________________________________________________

### Task 3: Measured Projection and Singleflight Cache

**Files:**

- Create: `graph/community_analysis.go`
- Create: `graph/community_analysis_test.go`
- Modify: `graph/community.go:12-237`
- Modify: `graph/community_test.go`
- Modify: `graph/memstore.go`, `graph/memstore_refs.go`, `graph/memstore_write.go`
- Modify: `graph/sqlite_graph.go`, `graph/sqlite_graph_refs.go`
- Modify: `graph/composite.go`, `graph/capabilities.go`, `cmd/serve_registry.go`

**Interfaces:**

- Produces `ProjectionStats`, `CommunityOptions`, `CommunityAnalysis`,
  `CommunityAnalysisCache`, `CommunityAnalyzerProvider`, and
  `CloneCommunityResult`.

- [ ] **Step 1: Write the failing projection-work test**

```go
func TestDetectCommunities_ReportsProjectionWork(t *testing.T) {
	got := DetectCommunities(map[string][]string{
		"A": {"n1", "n2", "n3"}, "B": {"n2", "n3"},
	}, 2)
	assert.Equal(t, uint64(5), got.Projection.PairCandidates)
	assert.Equal(t, uint64(3), got.Projection.FinalEdges)
	assert.Equal(t, uint64(3), got.Projection.Nodes)
	assert.Equal(t, uint64(2), got.Projection.Tokens)
}
```

- [ ] **Step 2: Implement measured projection**

```go
type ProjectionStats struct {
	Tokens uint64 `json:"tokens"`
	Nodes uint64 `json:"nodes"`
	PairCandidates uint64 `json:"pair_candidates"`
	FinalEdges uint64 `json:"final_edges"`
}
```

Add it to `CommunityResult`. `buildProjectionMeasured` increments candidates
before the self-edge guard and counts distinct undirected adjacency entries.
Keep the old-signature `buildProjection` wrapper for internal tests and
`ConnectedComponents`.

- [ ] **Step 3: Write cache/singleflight/invalidation tests**

```go
func TestCommunityAnalysisCache_ReusesContentAndSeparatesOptions(t *testing.T) {
	var computes atomic.Int32
	c := newCommunityAnalysisCache(func(refs map[string][]string, minSize int) *CommunityResult {
		computes.Add(1)
		return DetectCommunities(refs, minSize)
	})
	load := func(context.Context) (*RefSnapshot, error) {
		return SnapshotAllRefs(map[string][]string{"T": {"a", "b"}}), nil
	}
	a, err := c.Analyze(context.Background(), "all", CommunityOptions{MinCommunitySize: 2}, load)
	require.NoError(t, err)
	b, err := c.Analyze(context.Background(), "all", CommunityOptions{MinCommunitySize: 2}, load)
	require.NoError(t, err)
	assert.False(t, a.CacheHit)
	assert.True(t, b.CacheHit)
	assert.Equal(t, int32(1), computes.Load())
	_, err = c.Analyze(context.Background(), "all", CommunityOptions{MinCommunitySize: 2, ExcludeTests: true}, load)
	require.NoError(t, err)
	assert.Equal(t, int32(2), computes.Load())
	c.Clear()
	_, err = c.Analyze(context.Background(), "all", CommunityOptions{MinCommunitySize: 2}, load)
	require.NoError(t, err)
	assert.Equal(t, int32(3), computes.Load())
}
```

Add a 16-goroutine test with a blocking detector and assert one computation.
Add a test where the loader returns a changed content digest without calling
`Clear`; assert the next request misses. Add a MemoryStore mutation test
asserting `AddRef` and `DeleteFileNodes` also turn the next request into a miss.
Run these under `-race`.

- [ ] **Step 4: Implement immutable cached analysis**

```go
type CommunityOptions struct {
	MinCommunitySize int
	ExcludeTests bool
}
type CommunityAnalysis struct {
	Snapshot *RefSnapshot
	Result *CommunityResult
	CacheHit bool
}
type CommunityAnalyzerProvider interface {
	AnalyzeCommunities(context.Context, *RefRestriction, CommunityOptions) (*CommunityAnalysis, error)
	ClearCommunityAnalysis()
}
```

Load the selected snapshot before lookup, then key by algorithm version,
restriction/whole sentinel, `RefSnapshot.ContentDigest`, minimum size, and
`ExcludeTests`. Run filter/detect inside singleflight and store one immutable
result; return a per-call wrapper for `CacheHit`. This intentionally permits a
warm SQLite call to scan/hash its selected rows again: that cost is linear and
keeps externally updated databases exact, while the pair expansion and Louvain
work are reused. Embed the cache in MemoryStore, SQLiteGraph, and CompositeGraph.
Clear eagerly on MemoryStore ref mutations/deletion, legacy SQLite AddRef, and
Composite mount changes. Delegate through lazyGraph. Implement a deep
`CloneCommunityResult` for consumers.

- [ ] **Step 5: Verify and commit**

```bash
go test ./graph -run 'TestDetectCommunities_Reports|TestCommunityAnalysis' -count=1
go test -race ./graph -run 'TestCommunityAnalysisCache|TestCommunityAnalysis_CallerMutation' -count=1
go test ./graph -count=1
git add graph/community_analysis.go graph/community_analysis_test.go graph/community.go graph/community_test.go graph/memstore.go graph/memstore_refs.go graph/memstore_write.go graph/sqlite_graph.go graph/sqlite_graph_refs.go graph/composite.go graph/capabilities.go cmd/serve_registry.go
git commit -m '[mache-b37cff] perf(graph): cache measured community projections'
```

______________________________________________________________________

### Task 4: Scoped MCP Consumers

**Files:**

- Create: `cmd/community_analysis.go`
- Create: `cmd/community_restriction_test.go`
- Modify: `cmd/serve_handler_get_communities.go:27-241`
- Modify: `cmd/serve_diagram.go:12-74`
- Modify: `cmd/serve_architecture.go:98-160`
- Modify: `cmd/serve_handlers.go:113-221`
- Modify: `cmd/serve_test.go`

**Interfaces:**

- Consumes Task 3 provider/results.

- Produces optional `roots: string[]` on communities and diagrams, plus additive
  analysis metadata. Architecture stays global but shares the global cache.

- [ ] **Step 1: Write failing handler and registration tests**

```go
func TestGetCommunities_RootsReturnRestrictedWork(t *testing.T) {
	s := graph.NewMemoryStore()
	for _, id := range []string{"target/A", "target/B", "noise/X", "noise/Y"} {
		require.NoError(t, s.AddRef("Shared", id))
	}
	res, err := makeGetCommunitiesHandler(s)(context.Background(), makeRequest(map[string]any{
		"roots": []string{"target"}, "min_size": float64(2), "summary": true,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	var body struct {
		NumNodes int `json:"num_nodes"`
		Analysis struct {
			Roots []string `json:"roots"`
			RowsRead uint64 `json:"rows_read"`
			PairCandidates uint64 `json:"pair_candidates"`
		} `json:"analysis"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &body))
	assert.Equal(t, 2, body.NumNodes)
	assert.Equal(t, []string{"target"}, body.Analysis.Roots)
	assert.Equal(t, uint64(2), body.Analysis.RowsRead)
	assert.Equal(t, uint64(1), body.Analysis.PairCandidates)
}
```

Also test explicit empty roots returns a tool error, absent roots stays global,
and registration exposes a string-item array on communities/diagram only.

- [ ] **Step 2: Implement one request adapter and metadata shape**

```go
type communityAnalysisMetadata struct {
	Roots []string `json:"roots,omitempty"`
	RestrictionDigest string `json:"restriction_digest"`
	ContentDigest string `json:"content_digest"`
	RowsRead uint64 `json:"rows_read"`
	Tokens uint64 `json:"tokens"`
	Nodes uint64 `json:"nodes"`
	PairCandidates uint64 `json:"pair_candidates"`
	FinalEdges uint64 `json:"final_edges"`
	CacheHit bool `json:"cache_hit"`
}
```

Inspect raw request arguments to distinguish absent from explicit empty roots.
Require `CommunityAnalyzerProvider`; never fall back from an explicitly scoped
request to global refs.

- [ ] **Step 3: Wire handlers without mutating cache entries**

Communities uses `analysis.Snapshot.Refs` for sheaf topology and clones results
before response sorting/truncation. Diagram accepts roots and prefixes Mermaid
with a valid metadata comment. Architecture requests the unrestricted cached
analysis and sorts a copied community slice.

```text
%% mache-analysis {"roots":["target"],"rows_read":2,"pair_candidates":1,"cache_hit":true}
```

- [ ] **Step 4: Register roots arrays**

```go
mcp.WithArray("roots",
	mcp.Description("Optional projected graph roots; exact-or-descendant induced subgraph with no implicit boundary expansion."),
	mcp.WithStringItems(mcp.MinLength(1)),
)
```

- [ ] **Step 5: Prove cross-handler reuse, verify, and commit**

Use a counting provider to call communities, diagram, then architecture with
unrestricted/min-size-2 options and assert one underlying computation. Assert
the first response remains byte-stable after later handlers sort their copies.

```bash
go test ./cmd -run 'TestGetCommunities_Roots|TestCommunityHandlers|TestRegisterMCPTools' -count=1
go test -race ./cmd -run TestCommunityHandlers_ReuseOneAnalysis -count=1
git add cmd/community_analysis.go cmd/community_restriction_test.go cmd/serve_handler_get_communities.go cmd/serve_diagram.go cmd/serve_architecture.go cmd/serve_handlers.go cmd/serve_test.go
git commit -m '[mache-b37cff] feat(mcp): analyze projected subgraphs'
```

______________________________________________________________________

### Task 5: End-to-End Correctness and Performance Proof

**Files:**

- Create: `build/restriction_e2e_test.go`
- Create: `cmd/community_restriction_perf_test.go`
- Modify: `Taskfile.yml:491-633`
- Create: `benchmarks/restriction-first-community.md`

**Interfaces:**

- Consumes public `build.ParseWithSchemaRef`, `graph.Open`, handlers, and Mache's
  cached real-corpus database.

- Produces global/scoped × cold/warm JSON, pprof, flamegraphs, and report.

- [ ] **Step 1: Write the public-build equivalence/invariance test**

```go
func TestRestrictionFirstCommunity_PublicBuildEquivalentToTargetAlone(t *testing.T) {
	combined := writeRestrictionCorpus(t, 120)
	targetOnly := writeTargetOnlyCorpus(t)
	combinedDB, targetDB := filepath.Join(t.TempDir(), "combined.db"), filepath.Join(t.TempDir(), "target.db")
	require.NoError(t, build.ParseWithSchemaRef(combined, combinedDB, "go", combined))
	require.NoError(t, build.ParseWithSchemaRef(targetOnly, targetDB, "go", targetOnly))
	full, err := graph.Open(combinedDB)
	require.NoError(t, err)
	defer full.Close()
	target, err := graph.Open(targetDB)
	require.NoError(t, err)
	defer target.Close()
	scoped, err := full.RefsWithin(context.Background(), graph.RefRestriction{Roots: []string{"target"}})
	require.NoError(t, err)
	want := graph.SnapshotAllRefs(target.RefsMap())
	assert.Equal(t, want.Refs, scoped.Refs)
	assert.Equal(t, want.ContentDigest, scoped.ContentDigest)
	gotC, wantC := graph.DetectCommunities(scoped.Refs, 2), graph.DetectCommunities(want.Refs, 2)
	assert.Equal(t, wantC.Communities, gotC.Communities)
	assert.Equal(t, wantC.Membership, gotC.Membership)
	assert.InDelta(t, wantC.Modularity, gotC.Modularity, 1e-12)
	assert.Greater(t, graph.DetectCommunities(full.RefsMap(), 2).Projection.PairCandidates, gotC.Projection.PairCandidates)
}
```

Generate valid packages `target` and `noise`; each noise function calls one
shared function. Build another combined corpus with 240 noise functions and
assert scoped restriction/content digests, rows, pair candidates, communities,
and quotient output remain unchanged.

- [ ] **Step 2: Add one-database Mache-on-Mache profiler**

Call `testfixtures.Get(t, "mache-self")` once and run:

```go
cases := []restrictionProfileCase{
	{Name: "community-global-cold", Roots: nil, ClearBefore: true},
	{Name: "community-global-warm", Roots: nil, PrimeBefore: true},
	{Name: "community-graph-cold", Roots: []string{"graph"}, ClearBefore: true},
	{Name: "community-graph-warm", Roots: []string{"graph"}, PrimeBefore: true},
}
```

Record latency, total allocations, mallocs, selected rows/tokens/nodes, pair
candidates, edges, digest, and cache hit. Capture single-call CPU/heap profiles
when enabled. Assert scoped cold has fewer rows/pairs, cold misses, warm hits,
and each cold/warm pair shares a content digest. Do not assert time or bytes.

- [ ] **Step 3: Add the reproducible Task target**

```yaml
profile-community-restrictions:
  desc: Profile global/scoped and cold/warm communities on mache itself
  cmds:
    - mkdir -p {{.ROOT_DIR}}/.e2e/pprof
    - MACHE_E2E_LARGE=1 E2E_CAPTURE_PPROF=1 E2E_PPROF_DIR={{.ROOT_DIR}}/.e2e/pprof E2E_PROFILE_OUT={{.ROOT_DIR}}/.e2e/restriction-profile.json go test -tags boltdb -run '^TestE2E_RestrictionFirstCommunity_MacheOnMache$' -count=1 -v ./cmd/
```

- [ ] **Step 4: Run and inspect profiles**

```bash
go test ./build -run '^TestRestrictionFirstCommunity' -count=1 -v
task profile-community-restrictions
task flamegraphs
go tool pprof -top .e2e/pprof/community-global-cold.cpu.pprof
go tool pprof -top .e2e/pprof/community-graph-cold.cpu.pprof
go tool pprof -alloc_space -top .e2e/pprof/community-global-cold.heap.pprof
go tool pprof -alloc_space -top .e2e/pprof/community-graph-cold.heap.pprof
```

Create a clean baseline worktree at merged PR #622 (`7e3b8e8`) using the
`superpowers:using-git-worktrees` skill. Run the same existing large-tier matrix
once on the baseline and once on the feature commit, writing manifests/profiles
to distinct `/tmp/mache-b37cff-{baseline,feature}` directories:

```bash
MACHE_E2E_LARGE=1 E2E_CAPTURE_PPROF=1 E2E_CPU_ITERATIONS=1 E2E_PROFILE_OUT=/tmp/mache-b37cff-baseline/tool-profile.json E2E_PPROF_DIR=/tmp/mache-b37cff-baseline/pprof go test -tags boltdb -run '^TestE2E_MacheOnMache$' -count=1 -v ./cmd/
MACHE_E2E_LARGE=1 E2E_CAPTURE_PPROF=1 E2E_CPU_ITERATIONS=1 E2E_PROFILE_OUT=/tmp/mache-b37cff-feature/tool-profile.json E2E_PPROF_DIR=/tmp/mache-b37cff-feature/pprof go test -tags boltdb -run '^TestE2E_MacheOnMache$' -count=1 -v ./cmd/
```

The four-case same-database test is the causal scope/cache comparison. The
parent/feature manifests are a secondary regression check because their source
trees differ by the implementation patch.

- [ ] **Step 5: Write the measured report and commit**

Record exact commit/OS/arch/Go/LLO/corpus, four unrounded same-database rows, the
baseline/feature handler rows, artifact filenames, commands, leading
CPU/allocation stacks, and whether scope removed pair expansion and cache
removed repeated projection. State non-claims: no module/global Louvain
equivalence, timing SLA, durable cache, or import closure.

```bash
git add build/restriction_e2e_test.go cmd/community_restriction_perf_test.go Taskfile.yml benchmarks/restriction-first-community.md
git commit -m '[mache-b37cff] test(perf): prove restriction-first communities'
```

______________________________________________________________________

### Task 6: Public Contract and Full Verification

**Files:**

- Modify: `README.md:31-40`
- Modify: `docs/ARCHITECTURE.md:125-145,198-214,375-390`
- Modify: `CHANGELOG.md`
- Modify: `server.json` (generated)

**Interfaces:**

- Consumes all tasks and measured results; produces documented/generated MCP
  contract.

- [ ] **Step 1: Update docs and changelog**

Document projected-path roots, absent/empty behavior, exact descendant matching,
no boundary expansion, SQL-before-Go pushdown, separate cold-scope/warm-cache
effects, both producers' `idx_refs_node`, global architecture reuse, measured
results, LLO non-work, and `ley-line-open-69305e` as durable follow-up.

- [ ] **Step 2: Regenerate MCP schema and run focused gates**

```bash
task gen:server-json
task gen:server-json:check
task fmt
git diff --check
go test ./graph ./build ./cmd -count=1
task lint:actions
```

Expected: server.json exposes string-array roots on communities/diagram only.

- [ ] **Step 3: Run full and installed-binary verification**

```bash
task check
task install
task install:verify
task daemon:verify
task profile-community-restrictions
task flamegraphs
```

Expected: gates pass, installed Mache reports pinned LLO v0.18.2, daemon serves
the installed build, deterministic counters match, and profiles regenerate.

- [ ] **Step 4: Commit, push, and open the new PR**

```bash
git add README.md docs/ARCHITECTURE.md CHANGELOG.md server.json
git commit -m '[mache-b37cff] docs: publish restriction-first community proof'
git pull --rebase
git push -u origin design/mache-b37cff-restriction-first
gh pr create --base main --head design/mache-b37cff-restriction-first --title 'perf: execute community analysis over addressed restrictions' --body-file /tmp/mache-b37cff-pr.md
```

The PR body includes the four-case table, reproduction commands, deterministic
gates, flamegraph filenames, standalone index correction, LLO non-work decision,
and beads `mache-b37cff` / `ley-line-open-69305e`. Enable auto-merge only after
required checks are present and passing.

## Completion Criteria

- Scoped output equals a target-only build and ignores unrelated enlargement.
- Both SQLite producer shapes use `idx_refs_node`.
- Global/scoped and cold/warm measurements are separately reported.
- Three community consumers reuse identical per-process analysis.
- Response formatting never mutates cached results.
- Full, installed-binary, daemon, profile, and flamegraph verification passes.
- The new branch is pushed and its PR contains reproducible performance proof.
