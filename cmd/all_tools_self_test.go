package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentic-research/mache/graph"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/testfixtures"
)

// TestE2E_BackendParity_MacheOnMache — the "rough edges" gate.
//
// Bead mache-655e98: a regression in SQLiteGraph.DefsMap caused
// get_impact and get_architecture to return tiny empty envelopes on
// every corpus, every language. The pre-fix matrix runner DID exercise
// these tools and they DID return status=ok — they just returned wrong
// data. Happy-path passing said nothing.
//
// This test runs both backends against mache's own source and asserts
// that for a fixed set of tools whose results SHOULD agree to first
// order across backends, the response body sizes are within tolerance.
// Backends are allowed to differ in serialization order, trailing
// metadata, or precise node ordering, but if get_impact returns 51
// bytes on one backend and 2506 on the other, something is broken
// regardless of what status code each handler reported.
//
// Tolerance: ratio of smaller/larger ≥ 0.40. Loose because:
//
//   - MemoryStore aggregates AllDefsMap across all in-process state;
//     SQLiteGraph's snapshot is whatever node_defs holds at .db build
//     time. A 2× delta would be a legitimate fixture-shape difference.
//   - The 50× delta this test exists to catch is unambiguous; ratios
//     below 0.40 are guaranteed regressions, not data-shape variance.
//
// Tools currently exercised:
//
//   - get_impact     — caught the DefsMap fallback regression
//   - get_architecture — same code path; same regression surface
//
// Adding a tool here should be cheap: just append to parityTools.
// If a tool legitimately diverges by more than the tolerance (e.g.
// because one backend doesn't implement it), exclude it explicitly
// in the for-loop with a one-line comment naming the reason.
//
// Gated behind MACHE_E2E_SELF=1 same as TestE2E_MacheOnMache because
// the test requires a real-sized corpus to surface the deltas; on the
// 4-file toy fixture the get_impact regression is invisible.
func TestE2E_BackendParity_MacheOnMache(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e backend parity; rerun without -short")
	}
	if os.Getenv("MACHE_E2E_SELF") == "" {
		t.Skip("set MACHE_E2E_SELF=1 to run the backend-parity invariant")
	}

	t.Setenv("MACHE_NO_LEYLINE", "1")
	repoRoot := macheRepoRoot(t)
	schema, err := resolveSchema("go", ".")
	require.NoError(t, err)
	require.NotNil(t, schema, "go preset schema must resolve")

	opts := readPprofOpts(t)
	perBackend := make(map[string]map[string]toolProfile)
	for _, backend := range allE2EBackends() {
		g, cleanup := backend.build(t, repoRoot, schema)
		profiles := runToolMatrix(t, g, repoRoot, backend.name, opts)
		cleanup()
		perBackend[backend.name] = indexProfiles(profiles)
	}

	mem, hasMem := perBackend["memory"]
	sql, hasSQL := perBackend["sqlite"]
	require.True(t, hasMem, "memory backend profile must exist")
	require.True(t, hasSQL, "sqlite backend profile must exist")

	// Tools whose body size MUST agree to ≥40% across backends.
	// get_impact and get_architecture share the DefsMap traversal that
	// regressed in mache-655e98; the ratio there pre-fix was ~0.02.
	parityTools := []string{"get_impact", "get_architecture"}
	const minRatio = 0.40

	for _, name := range parityTools {
		m, mOK := mem[name]
		s, sOK := sql[name]
		require.True(t, mOK, "memory profile missing for %s", name)
		require.True(t, sOK, "sqlite profile missing for %s", name)

		// Both must be ok — a backend silently entering the "no
		// definition found" branch was the original regression
		// shape (status=ok, body=51B).
		require.Equal(t, "ok", m.Status, "memory %s must be ok; got %s (%s)", name, m.Status, m.Reason)
		require.Equal(t, "ok", s.Status, "sqlite %s must be ok; got %s (%s)", name, s.Status, s.Reason)

		smaller, larger := m.BodySize, s.BodySize
		if smaller > larger {
			smaller, larger = larger, smaller
		}
		require.Positive(t, larger, "%s: both backends returned 0-byte body — fixture or wiring broken", name)
		ratio := float64(smaller) / float64(larger)
		assert.GreaterOrEqual(t, ratio, minRatio,
			"%s body-size parity broken: memory=%d B, sqlite=%d B, ratio=%.3f (min %.2f). "+
				"Likely a backend regression in shared code path. See bead mache-655e98 for prior incident.",
			name, m.BodySize, s.BodySize, ratio, minRatio)
	}
}

// TestE2E_MacheOnMache — SB-06 (ADR-0017 M2).
//
// The synthetic 4-file fixture in TestE2E_AllMCPTools exercises the
// tool surface but says nothing about how the backends behave on a
// real corpus with deep package trees, mixed file sizes, vendored
// dependencies, and the long-tail shapes that surface only when the
// graph has thousands of nodes.
//
// This test ingests mache's own source tree (which the runner can
// always locate via runtime.Caller) and exercises the full read-side
// tool inventory against it on both backends. The acceptance bar
// goes beyond TestE2E_AllMCPTools's "no transport errors" check —
// per-tool assertions confirm that tools which SHOULD hit on a real
// mache repo actually return non-empty results.
//
// The shared invocation table in runToolMatrix uses symbol="Validate"
// and pattern="%alidate%" so the assertions follow suit:
//
//   - find_callers   token=Validate    → ≥1 caller
//   - find_definition symbol=Validate  → resolves
//   - get_overview                     → non-trivial body
//   - list_directory ""                → root has child dirs
//   - search          pattern=Validate → ≥1 hit
//
// Additional symbol probes (NewEngine, NewMemoryStore, etc.) require
// per-tool argument tables, deferred to SB-04 / SB-05 work.
//
// Tools whose semantics legitimately depend on LLO-only tables
// (semantic_search, get_type_info, get_diagnostics, find_smells with
// _ast rules) are allowed to return skipped; the matrix runner's
// existing classification handles that.
//
// Skipped under -short because ingest of a ~30K-LOC tree takes
// single-digit seconds per backend and the test is large enough to
// count as integration, not unit. The MACHE_E2E_SELF env gate is
// gone post-ADR-0019 — the fixture lives in the medium tier of the
// real-corpus fixture registry which is always-on.
func TestE2E_MacheOnMache(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e mache-on-mache; rerun without -short")
	}

	t.Setenv("MACHE_NO_LEYLINE", "1")
	testfixtures.RequireTier(t, "large")

	// Resolve fixture path + schema through the registry. The matrix
	// runner still builds per-backend graphs locally (memory + sqlite)
	// so backend deltas surface; the registry centralizes WHERE the
	// fixture lives, not HOW each backend ingests it.
	repoRoot, err := testfixtures.ResolvePath("mache-self")
	require.NoError(t, err, "resolve mache-self path")
	schema, err := testfixtures.LoadSchema("mache-self")
	require.NoError(t, err, "load mache-self schema")
	require.NotNil(t, schema, "go preset schema must resolve")

	opts := readPprofOpts(t)
	for _, backend := range allE2EBackends() {
		t.Run(backend.name, func(t *testing.T) {
			g, cleanup := backend.build(t, repoRoot, schema)
			defer cleanup()

			profiles := runToolMatrix(t, g, repoRoot, backend.name, opts)
			printProfileSummary(t, profiles)
			assertHarnessHealth(t, profiles)
			assertMacheOnMacheInvariants(t, profiles)
		})
	}
}

// macheRepoRoot returns the absolute path of mache's source root.
// Derived from runtime.Caller so the test works regardless of which
// checkout the developer has cd'd into (~/remotes/art/mache vs
// ~/github/art/mache).
func macheRepoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) must succeed")
	// cmd/all_tools_self_test.go → ../.. → repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(here), ".."))
	require.FileExists(t, filepath.Join(root, "go.mod"), "repo root must contain go.mod")
	return root
}

// assertMacheOnMacheInvariants applies SB-06's per-tool reality
// checks on top of the harness's no-transport-error bar. Asserts each
// tool that should hit on mache's actual source DID return ok with a
// non-trivial body.
func assertMacheOnMacheInvariants(t *testing.T, profiles []toolProfile) {
	t.Helper()
	byName := indexProfiles(profiles)

	// list_directory, get_overview, search — these are the
	// "graph is non-empty" smoke tools. If any of them is skipped
	// or returns 0 bytes on the mache repo, the ingest is broken,
	// not the tool. Fail loudly.
	for _, name := range []string{"list_directory", "get_overview", "search"} {
		p, ok := byName[name]
		require.True(t, ok, "profile missing for %s", name)
		require.Equal(t, "ok", p.Status, "%s must be ok on mache-on-mache; got %s (%s)", name, p.Status, p.Reason)
		assert.Positive(t, p.BodySize, "%s body must be non-empty", name)
	}

	// find_callers + find_definition should hit. The shared
	// invocation table runs them with symbol="Validate" — a name
	// that exists across both internal/ingest and cmd/. Additional
	// symbol probes would require per-corpus arg tables (SB-04/05).
	callers, ok := byName["find_callers"]
	require.True(t, ok)
	if callers.Status == "ok" {
		assert.Positive(t, callers.BodySize,
			"find_callers Validate must return at least one caller on mache repo")
	} else {
		t.Errorf("find_callers Validate must succeed on mache repo; got %s (%s)",
			callers.Status, callers.Reason)
	}

	def, ok := byName["find_definition"]
	require.True(t, ok)
	if def.Status == "ok" {
		assert.Positive(t, def.BodySize, "find_definition Validate must return a def on mache repo")
	} else {
		t.Errorf("find_definition Validate must succeed on mache repo; got %s (%s)",
			def.Status, def.Reason)
	}
}

// indexProfiles converts the profile slice to a name→profile map so
// invariant assertions can look up a tool by name without re-scanning.
func indexProfiles(profiles []toolProfile) map[string]toolProfile {
	out := make(map[string]toolProfile, len(profiles))
	for _, p := range profiles {
		out[p.Name] = p
	}
	return out
}

// TestE2E_RealCorpora — exercises the tool surface across distinct
// medium-tier fixtures in the registry. Default behavior (post-ADR-0019
// PR 2) iterates `defaultRealCorpusFixtures` and runs the matrix runner
// against each. No env var needed: medium tier is always-on per
// testfixtures.RequireTier. The mache-self fixture stays in the registry,
// but its stronger matrix already runs in TestE2E_MacheOnMache and is not
// repeated here.
//
// MACHE_E2E_CORPORA override (legacy / ad-hoc path):
//
//	MACHE_E2E_CORPORA="my-corpus=/path/to/repo,rust" \
//	  go test -run TestE2E_RealCorpora ./cmd/ -timeout 10m -v
//
// When set, the env var REPLACES the registry-driven set with the
// developer's ad-hoc paths. Used for testing against repos that
// haven't been snapshotted into testdata/snapshots/ yet (the snapshot
// workflow is `task fixtures:snapshot`; until a repo is snapshotted,
// pointing the test at a live working tree is the escape hatch).
//
// Each fixture yields a subtest under (fixture_id, backend). The test
// asserts only the harness-health bar (no transport errors, at least
// one ok tool) — per-tool reality checks vary per language and live
// in TestE2E_MacheOnMache for the Go case; per-language reality
// checks for Rust / polyglot are deferred to SB-04 / SB-05 work.
func TestE2E_RealCorpora(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e real-corpora; rerun without -short")
	}
	testfixtures.RequireTier(t, "large")
	t.Setenv("MACHE_NO_LEYLINE", "1")
	opts := readPprofOpts(t)

	// Env var override: ad-hoc paths replace the registry-driven set.
	// Documented escape hatch for repos that aren't snapshotted yet.
	if corpora := os.Getenv("MACHE_E2E_CORPORA"); corpora != "" {
		runRealCorporaFromEnv(t, corpora, opts)
		return
	}

	// Default path: iterate distinct medium-tier fixtures in the registry.
	// mache-self is covered by the stronger TestE2E_MacheOnMache matrix.
	for _, fx := range defaultRealCorpusFixtures() {
		t.Run(fx.ID, func(t *testing.T) {
			runRealCorpusFixture(t, fx, opts)
		})
	}
}

func defaultRealCorpusFixtures() []testfixtures.Fixture {
	var fixtures []testfixtures.Fixture
	for _, fx := range testfixtures.All() {
		if fx.Tier != "medium" || fx.ID == "mache-self" {
			continue
		}
		fixtures = append(fixtures, fx)
	}
	return fixtures
}

// runRealCorpusFixture executes the backend matrix for one registry
// fixture. Factored out of TestE2E_RealCorpora to keep the test body
// readable and to give the env-var override path a shared helper.
func runRealCorpusFixture(t *testing.T, fx testfixtures.Fixture, opts pprofOpts) {
	t.Helper()
	srcPath, err := testfixtures.ResolvePath(fx.ID)
	require.NoError(t, err, "resolve path for %s", fx.ID)
	schema, err := testfixtures.LoadSchema(fx.ID)
	require.NoError(t, err, "load schema for %s", fx.ID)
	require.NotNil(t, schema, "schema for %s must resolve", fx.ID)

	for _, backend := range allE2EBackends() {
		t.Run(backend.name, func(t *testing.T) {
			g, cleanup := backend.build(t, srcPath, schema)
			defer cleanup()

			profiles := runToolMatrix(t, g, srcPath, backend.name, opts)
			printProfileSummary(t, profiles)
			// Loose: only the harness-health bar applies here.
			// Per-tool reality checks would need per-corpus arg
			// tables, deferred to SB-04 (hetero) / SB-05 (synth).
			assertHarnessHealth(t, profiles)
		})
	}
}

// runRealCorporaFromEnv handles the MACHE_E2E_CORPORA legacy/ad-hoc
// override. Same matrix-runner behavior as the registry path; just
// sources paths from the env-var spec instead of the manifest.
func runRealCorporaFromEnv(t *testing.T, corpora string, opts pprofOpts) {
	t.Helper()
	for _, entry := range strings.Split(corpora, ":") {
		name, path, schemaName, ok := parseCorpusSpec(entry)
		if !ok {
			t.Errorf("invalid corpus spec %q (expected name=path[,schema])", entry)
			continue
		}
		path = expandHome(path)
		if _, err := os.Stat(path); err != nil {
			t.Logf("skipping %s: %v", name, err)
			continue
		}

		schema, err := resolveSchema(schemaName, path)
		require.NoError(t, err, "resolve schema %q for %s", schemaName, name)
		require.NotNil(t, schema, "schema %q for %s", schemaName, name)

		t.Run(name, func(t *testing.T) {
			for _, backend := range allE2EBackends() {
				t.Run(backend.name, func(t *testing.T) {
					g, cleanup := backend.build(t, path, schema)
					defer cleanup()

					profiles := runToolMatrix(t, g, path, backend.name, opts)
					printProfileSummary(t, profiles)
					assertHarnessHealth(t, profiles)
				})
			}
		})
	}
}

// TestFindSmells_DeadCode_PerfGate_MacheOnMache — bead mache-68980e.
//
// Background: the original dead_code rule's `alive` CTE joined v_defs
// to v_refs with an OR-ed predicate ("target_node_id match OR token
// match OR receiver-strip match"). SQLite's planner cannot use an
// index when JOIN conditions are joined by top-level OR — it falls
// back to a nested-loop scan over the cross product. On the mache-on-
// mache corpus that's ~3K defs × ~45K refs = ~135M pairs, observed as
// 57 seconds wall-clock for the rule (alive CTE alone: 12s).
//
// Fix (mache-68980e): split the OR-join into three UNION arms. Each
// arm has one equality predicate the planner can satisfy via an index
// on v_refs.target_node_id (L_1 binding arm) or v_refs.token (L_0
// mention + receiver-method arms). Same DISTINCT semantics; same
// findings; ~10ms for the alive CTE on the same .db.
//
// Acceptance bar: < 5 seconds total wall-clock for the rule on the
// mache-on-mache .db. Why 5s and not "as fast as possible":
//
//   - The original 57s was unworkable for any pre-commit / CI gate —
//     a developer running `mache find-smells --rule dead_code` before
//     pushing waited a full minute for feedback on a rule that should
//     be a fast structural pattern check.
//   - 5s is the threshold where the rule becomes usable in a CI
//     workflow without timing out test runs. Pre-commit hooks
//     typically budget ~10s total for all checks; a single rule
//     consuming half that budget is still acceptable, more is not.
//   - The actual post-fix timing on a 353-file Go corpus runs ~1-2s
//     (see commit message for measurements); 5s gives ~3× headroom
//     for slower hardware (CI runners, ARM emulation) so the gate
//     doesn't flake on legitimate variance.
//
// Skipped under -short because the build step ingests mache's own
// source tree (single-digit seconds on a developer laptop). The
// MACHE_E2E_SELF env gate is gone post-ADR-0019 — the underlying
// fixture is the medium tier of the real-corpus fixture registry,
// always-on.
//
// ADR-0019 PR 3 update: the hardcoded 5s budget is replaced by the
// fixed-anchor + tolerance band gate in testdata/snapshots/baselines.toml.
// Current baseline is 124ms +25% (= 155ms ceiling on this rule for
// mache-self). Bumping the baseline is an explicit + audited action:
// `task fixtures:rebaseline key=find_smells:dead_code fixture=mache-self
//
//	wall_ms=<n> justification="<reason>"`. See ADR-0019 D.6 for why
//
// auto-rebaseline was rejected (perf rot ratchet).
func TestFindSmells_DeadCode_PerfGate_MacheOnMache(t *testing.T) {
	if testing.Short() {
		t.Skip("perf gate; rerun without -short")
	}
	if raceEnabled {
		// Race detector instruments every SQLite vtab read; measured
		// wall-clock is 10-20× the un-instrumented value (PR #409
		// integration matrix: 83s under -race vs ~3.5s without).
		// Any fixed-anchor baseline is meaningless under -race —
		// you're measuring synchronization overhead, not the rule.
		t.Skip("perf gate disabled under -race; race detector skews SQLite timings beyond any meaningful baseline")
	}

	t.Setenv("MACHE_NO_LEYLINE", "1")
	testfixtures.RequireTier(t, "large")

	// Pull the cached SQLiteGraph from the registry. First call in the
	// cmd test binary triggers ingest (~5s); subsequent tests reuse
	// the same .db. Cleanup is process-lifetime (see registry docs).
	g := testfixtures.Get(t, "mache-self")
	qg := graph.RefsQuerier(g) // *SQLiteGraph implements graph.RefsQuerier directly.

	rule := findRegisteredRule(t, "dead_code")

	start := time.Now()
	findings, err := runSmellRule(qg, rule, "" /*sourceID*/, 5000 /*limit*/)
	elapsed := time.Since(start)
	require.NoError(t, err, "dead_code rule must execute without error on mache-on-mache")

	// Surface finding count for diagnostic context before the band
	// check fires. AssertWithinBaseline emits its own pass/fail log.
	t.Logf("dead_code on mache-on-mache: %s (%d findings)", elapsed, len(findings))

	// Per ADR-0019 D.6: gate fails if measured > baseline * (1 + tolerance).
	// Anchor + tolerance live in testdata/snapshots/baselines.toml; the
	// helper t.Fatalfs with both numbers and a `task fixtures:rebaseline`
	// command if the regression is intentional.
	testfixtures.AssertWithinBaseline(t, "find_smells:dead_code", "mache-self", elapsed)
}

// TestE2E_RealCorpora_RegistryDrivenByDefault asserts the fixture registry
// retains both the mache-self sentinel and the distinct Rust corpus rather
// than falling back to the legacy MACHE_E2E_CORPORA env var.
//
// This is a structural check, not a behavioral one — we can't ride
// through testing.T.Run dynamically to verify the subtests were
// created without massively expanding the test surface. This test pins
// registry membership; DefaultSelectionDoesNotDuplicateMacheSelf below pins
// which registered fixtures the real-corpora matrix executes after the
// stronger standalone sentinel has covered mache-self.
func TestE2E_RealCorpora_RegistryDrivenByDefault(t *testing.T) {
	want := map[string]bool{
		"mache-self":         false,
		"medium-rust-rosary": false,
	}
	for _, fx := range testfixtures.All() {
		if fx.Tier != "medium" {
			continue
		}
		if _, expected := want[fx.ID]; expected {
			want[fx.ID] = true
		}
	}
	for id, found := range want {
		assert.True(t, found,
			"medium-tier fixture %q must remain in the registry",
			id)
	}
}

func TestE2E_RealCorpora_DefaultSelectionDoesNotDuplicateMacheSelf(t *testing.T) {
	var selected []string
	for _, fx := range defaultRealCorpusFixtures() {
		selected = append(selected, fx.ID)
	}

	assert.NotContains(t, selected, "mache-self",
		"TestE2E_MacheOnMache already runs the stronger mache-self matrix")
	assert.Contains(t, selected, "medium-rust-rosary",
		"distinct medium-tier corpora must remain in the default matrix")
}

func parseCorpusSpec(spec string) (name, path, schema string, ok bool) {
	name, rest, ok := strings.Cut(spec, "=")
	if !ok || name == "" || rest == "" {
		return "", "", "", false
	}
	if p, s, hasSchema := strings.Cut(rest, ","); hasSchema {
		return name, p, s, true
	}
	return name, rest, "go", true
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
