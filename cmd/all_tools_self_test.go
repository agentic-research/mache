package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// Skipped under -short and behind MACHE_E2E_SELF=1 because ingest of
// a ~30K-LOC tree takes single-digit seconds per backend and the test
// is large enough to count as integration, not unit.
func TestE2E_MacheOnMache(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e mache-on-mache; rerun without -short")
	}
	if os.Getenv("MACHE_E2E_SELF") == "" {
		t.Skip("set MACHE_E2E_SELF=1 to run the mache-on-mache invariant")
	}

	t.Setenv("MACHE_NO_LEYLINE", "1")

	repoRoot := macheRepoRoot(t)
	schema, err := resolveSchema("go", ".")
	require.NoError(t, err)
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

// TestE2E_RealCorpora — ad-hoc 5xxx-step driver. Ingests the
// developer's other repos when MACHE_E2E_CORPORA is set, exercising
// the tool surface against real polyglot trees outside mache's own
// source. Format: `MACHE_E2E_CORPORA=name=path[,schema]:name2=path2`.
// `schema` defaults to "go" when omitted (per parseCorpusSpec) —
// pass an explicit preset (rust/python/typescript/...) for non-Go
// corpora. Auto-detection from sentinel files is NOT applied here.
//
// Example:
//
//	MACHE_E2E_CORPORA="rosary=~/remotes/art/rosary,rust:llo=~/remotes/art/ley-line-open,rust" \
//	  go test -run TestE2E_RealCorpora ./cmd/ -timeout 10m -v
//
// Each entry yields a subtest under (corpus_name, backend). Failures
// here represent real corpus shapes that mache choked on — file a
// bead and either fix the tool or document the divergence. The test
// itself only asserts the harness-health bar (no transport errors,
// at least one ok) because tool semantics vary per language.
func TestE2E_RealCorpora(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e real-corpora; rerun without -short")
	}
	corpora := os.Getenv("MACHE_E2E_CORPORA")
	if corpora == "" {
		t.Skip("set MACHE_E2E_CORPORA=name=path,schema[:name2=path2] to exercise real corpora")
	}

	t.Setenv("MACHE_NO_LEYLINE", "1")
	opts := readPprofOpts(t)

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
					// Loose: only the harness-health bar applies here.
					// Per-tool reality checks would need per-corpus arg
					// tables, deferred to SB-04 (hetero) / SB-05 (synth).
					assertHarnessHealth(t, profiles)
				})
			}
		})
	}
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
