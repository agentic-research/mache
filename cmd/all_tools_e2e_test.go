package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

// E2E tool harness — mache-6b6da6 phase 1.
//
// One test per package that exercises every registered MCP tool against
// a realistic in-tree fixture and records per-tool latency + allocation
// profile. Two outputs:
//
//   - stdout: human-readable summary table (always)
//   - testdata/e2e_tool_profile.json: machine-readable manifest (gated
//     on E2E_PROFILE_OUT=path env var)
//
// Skipped under -short because the fixture build + tool invocations
// take seconds, not milliseconds. The point of this harness is observ-
// ability over the whole tool surface, not unit-level correctness —
// individual tool semantics are pinned by the per-handler test files.
//
// What this is NOT (yet):
//   - flamegraphs: phase 2 will add pprof CPU/heap capture per tool
//     and emit SVG via `go tool pprof`. Phase 1 is the harness.
//   - regression detection: phase 4 compares against a checked-in
//     baseline. Phase 1 just emits the manifest.
//
// What this IS:
//   - one place that proves every tool can be called against a
//     realistic graph without panic-ing or errorring on a happy path
//   - per-tool latency / alloc deltas, comparable across runs
//   - shape that future tools just append a row to

// toolProfile is one row of the e2e manifest.
type toolProfile struct {
	Name       string `json:"name"`
	LatencyMS  int64  `json:"latency_ms"`
	AllocBytes uint64 `json:"alloc_bytes"`
	AllocCount uint64 `json:"alloc_count"`
	// Result classification — exactly one of these will be set on a
	// successful run; harness failures (panic, transport error) fail
	// the test outright.
	Status   string `json:"status"`              // ok | tool_error | skipped
	Reason   string `json:"reason,omitempty"`    // when Status != ok
	BodySize int    `json:"body_size,omitempty"` // bytes, ok-status only
	// Optional pprof artifacts (phase 2). Empty string when capture
	// is disabled (E2E_CAPTURE_PPROF unset). Path is relative to
	// the test's cwd at run time; the manifest writer leaves it as
	// emitted so consumers (flamegraph step, dashboards) can resolve
	// against ROOT_DIR or the manifest's parent.
	CPUProfile  string `json:"cpu_profile,omitempty"`
	HeapProfile string `json:"heap_profile,omitempty"`
	// CPUIterations is the count of handler calls run under the
	// CPU profile boundary. Single-call profiles are too short for
	// the runtime sampler (~100Hz, 10ms tick) to record meaningful
	// stacks, so the harness loops K times inside one profile to
	// give the sampler something to chew on. K defaults to 50 and
	// is overridable via E2E_CPU_ITERATIONS.
	CPUIterations int `json:"cpu_iterations,omitempty"`
}

// pprofOpts is the per-test-run pprof capture configuration.
// Empty .dir means "do not capture pprof" — phase 1 default.
type pprofOpts struct {
	dir        string // .e2e/ or whatever; empty disables capture
	iterations int    // CPU profile loop count (heap is single-shot)
}

// readPprofOpts parses the env-var-driven pprof config. Returns
// zero-value opts (no capture) when E2E_CAPTURE_PPROF is unset.
func readPprofOpts(t *testing.T) pprofOpts {
	t.Helper()
	if os.Getenv("E2E_CAPTURE_PPROF") != "1" {
		return pprofOpts{}
	}
	dir := os.Getenv("E2E_PPROF_DIR")
	if dir == "" {
		// Default to a sibling of the manifest, or to a tempdir if
		// neither is set. Prefer co-locating with the manifest so
		// `task profile-tools` ends up with one tidy output dir.
		if mf := os.Getenv("E2E_PROFILE_OUT"); mf != "" {
			dir = filepath.Join(filepath.Dir(mf), "pprof")
		} else {
			dir = t.TempDir()
		}
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Default iterations: 500. Mache's tools complete in <1ms each
	// (phase 1 baseline), and the runtime CPU sampler runs at
	// ~100Hz (10ms tick). Below ~200 iterations the CPU profile
	// records zero samples — the loop exits before any tick.
	// 500 × ~1ms ≈ 500ms per tool, so a full pprof run takes
	// ~8s for the 16-tool surface. Heap profiling has no
	// equivalent sampling-rate constraint and is meaningful at
	// any iteration count; CPU is the dimensioning concern.
	//
	// On larger fixtures with slower tools, lower iterations work;
	// users can tune via E2E_CPU_ITERATIONS.
	iterations := 500
	if v := os.Getenv("E2E_CPU_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err, "E2E_CPU_ITERATIONS must be int")
		require.Positive(t, n, "E2E_CPU_ITERATIONS must be > 0")
		iterations = n
	}
	t.Logf("pprof capture enabled: dir=%s cpu_iterations=%d", dir, iterations)
	return pprofOpts{dir: dir, iterations: iterations}
}

// TestE2E_AllMCPTools is the harness. See file header.
func TestE2E_AllMCPTools(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e harness; rerun without -short")
	}

	// Pin to the in-process MemoryStore path so the fixture build is
	// deterministic. Auto-leyline would need a leyline binary on
	// PATH and would change the .db shape per environment — not what
	// this harness is testing.
	t.Setenv("MACHE_NO_LEYLINE", "1")

	dir := writeE2EFixture(t)
	// Load the go preset schema so the graph projects functions/,
	// types/, etc. — the schema-projected paths the tool args
	// reference. Empty topology builds a degenerate graph where
	// most tools have nothing to do (uncovered by the harness's
	// previous bare-topology run).
	schema, err := resolveSchema("go", ".")
	require.NoError(t, err)
	require.NotNil(t, schema, "go preset schema must resolve")
	g, cleanup, err := buildMaybeMultiGraph(dir, schema)
	require.NoError(t, err)
	defer cleanup()

	// Each entry: tool name, handler factory, args. Order matches
	// registerMCPTools (cmd/serve_handlers.go) so a missing tool
	// here is visually obvious. Args chosen to produce a non-trivial
	// response on the fixture — bare token "Validate" or path
	// "pkg/auth/main.go" has a known target.
	invocations := []struct {
		name    string
		handler func(graph.Graph) server.ToolHandlerFunc
		args    map[string]any
	}{
		{"list_directory", makeListDirHandler, map[string]any{"path": ""}},
		{"read_file", makeReadFileHandler, map[string]any{"path": "auth/functions/Validate/source"}},
		{"find_callers", makeFindCallersHandler, map[string]any{"token": "Validate"}},
		{"find_callees", makeFindCalleesHandler, map[string]any{"path": "billing/functions/Charge"}},
		{"search", makeSearchHandler, map[string]any{"pattern": "%alidate%"}},
		{"semantic_search", makeSemanticSearchHandler, map[string]any{"query": "auth"}},
		{"get_communities", makeGetCommunitiesHandler, map[string]any{"min_size": float64(2)}},
		{"find_definition", makeFindDefinitionHandler, map[string]any{"symbol": "Validate"}},
		{"get_type_info", makeGetTypeInfoHandler, map[string]any{"symbol": "Validate"}},
		{"get_diagnostics", makeGetDiagnosticsHandler, map[string]any{"limit": float64(10)}},
		{"get_overview", makeGetOverviewHandler, map[string]any{}},
		{"get_impact", makeGetImpactHandler, map[string]any{"symbol": "Validate", "depth": float64(2)}},
		{"get_architecture", makeGetArchitectureHandler, map[string]any{}},
		{"get_diagram", makeGetDiagramHandler, map[string]any{"layout": "TD"}},
		{"resolve_ref", makeResolveRefHandler, map[string]any{"token": "mod:./billing", "base_path": dir}},
		{"find_smells", makeFindSmellsHandler, map[string]any{"rule": "dead_code", "limit": float64(20)}},
		// write_file is intentionally skipped — happy path mutates
		// the fixture and would invalidate downstream tool results
		// in this single-pass harness. A separate write-path harness
		// is the right fit. Phase 1 pins the read surface.
	}

	opts := readPprofOpts(t)
	profiles := make([]toolProfile, 0, len(invocations))
	for _, inv := range invocations {
		profiles = append(profiles, profileTool(t, inv.name, inv.handler(g), inv.args, opts))
	}

	printProfileSummary(t, profiles)
	if out := os.Getenv("E2E_PROFILE_OUT"); out != "" {
		writeProfileManifest(t, out, profiles)
	}

	// Acceptance is intentionally loose: this harness exists to
	// emit observability over the tool surface, not to gate merges.
	// A tool returning IsError on a no-LLO fixture (e.g. find_smells
	// with _ast rules, get_type_info without _lsp_*) is documented
	// behavior — surfaced as "skipped" — not a failure. Tests for
	// individual semantics live in the per-handler test files.
	//
	// The single bar: NO tool errors out at the transport level
	// (panic, nil result, raw error) and AT LEAST ONE tool returns
	// ok. Anything below that is a harness misconfiguration or a
	// fundamental graph-build break, not legitimate tool semantics.
	var okCount, errorCount int
	for _, p := range profiles {
		switch p.Status {
		case "ok":
			okCount++
		case "tool_error":
			errorCount++
		}
	}
	require.Zero(t, errorCount,
		"at least one tool returned a transport-level error — harness misconfigured or graph build broken")
	require.Positive(t, okCount,
		"NO tools returned ok — likely fixture drift or fundamental graph-build break")
}

// profileTool calls one tool handler and records latency + alloc
// deltas. Forces a GC before the call so the alloc delta reflects
// only the tool's work, not the previous tool's still-live objects.
//
// When opts.dir is set, additionally captures CPU + heap pprof
// artifacts. The latency/alloc measurement still comes from a
// single canonical call so the manifest numbers are comparable
// run-over-run regardless of pprof being on. The CPU profile is
// captured over a separate K-iteration loop because single-call
// profiles are too short for the runtime sampler (~100Hz tick) to
// record meaningful stacks.
func profileTool(t *testing.T, name string, h server.ToolHandlerFunc, args map[string]any, opts pprofOpts) toolProfile {
	t.Helper()
	var startMem, endMem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&startMem)

	start := time.Now()
	res, err := h(context.Background(), makeRequest(args))
	elapsed := time.Since(start)

	runtime.ReadMemStats(&endMem)

	p := toolProfile{
		Name:       name,
		LatencyMS:  elapsed.Milliseconds(),
		AllocBytes: endMem.TotalAlloc - startMem.TotalAlloc,
		AllocCount: endMem.Mallocs - startMem.Mallocs,
	}
	switch {
	case err != nil:
		// Transport-level failure — should be rare; tools usually
		// return ok+IsError instead. Fail soft and surface the
		// error in the manifest so the harness still produces a
		// complete picture.
		p.Status = "tool_error"
		p.Reason = err.Error()
	case res == nil:
		p.Status = "tool_error"
		p.Reason = "handler returned nil result without error"
	case res.IsError:
		// Handlers return IsError for things like "no _ast table"
		// (LLO-required tool against a no-LLO fixture) or "no LSP
		// daemon for semantic_search." Distinguish from harness
		// failures by treating it as "skipped" — the tool's
		// surface is alive, it just had nothing to do.
		p.Status = "skipped"
		p.Reason = resultText(t, res)
		if len(p.Reason) > 200 {
			p.Reason = p.Reason[:200] + "..."
		}
	default:
		p.Status = "ok"
		p.BodySize = len(resultText(t, res))
	}

	// Phase 2: optional pprof capture. Skip on tools that errored
	// or were skipped — profiling an LSP-tool that immediately
	// returns "no _lsp_* table" is profile noise, not signal.
	if opts.dir != "" && p.Status == "ok" {
		capturePprof(t, name, h, args, opts, &p)
	}
	return p
}

// capturePprof writes <name>.cpu.pprof + <name>.heap.pprof under
// opts.dir. CPU profile spans a K-iteration loop; heap is a single
// snapshot taken after the loop. Sets p.CPUProfile / p.HeapProfile
// paths on the toolProfile so the manifest links them.
//
// Errors are logged via t.Logf but don't fail the test — the
// harness's primary job is the latency/alloc measurement; pprof
// is a bonus. A slot machine I/O failure shouldn't break the
// observability story.
func capturePprof(t *testing.T, name string, h server.ToolHandlerFunc, args map[string]any, opts pprofOpts, p *toolProfile) {
	t.Helper()

	// CPU profile over K iterations.
	cpuPath := filepath.Join(opts.dir, name+".cpu.pprof")
	cpuFile, err := os.Create(cpuPath)
	if err != nil {
		t.Logf("pprof: could not create %s: %v", cpuPath, err)
		return
	}
	defer func() { _ = cpuFile.Close() }()

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Logf("pprof: StartCPUProfile %s: %v", name, err)
		return
	}
	for i := 0; i < opts.iterations; i++ {
		// Ignore result/error: the canonical call above already
		// recorded ok-status. Iterations exist to give the sampler
		// enough work to record stacks.
		_, _ = h(context.Background(), makeRequest(args))
	}
	pprof.StopCPUProfile()
	p.CPUProfile = cpuPath
	p.CPUIterations = opts.iterations

	// Heap snapshot after the loop. GC first so the snapshot
	// reflects steady-state allocations rather than interim
	// garbage from the iteration loop.
	runtime.GC()
	heapPath := filepath.Join(opts.dir, name+".heap.pprof")
	heapFile, err := os.Create(heapPath)
	if err != nil {
		t.Logf("pprof: could not create %s: %v", heapPath, err)
		return
	}
	defer func() { _ = heapFile.Close() }()
	if err := pprof.WriteHeapProfile(heapFile); err != nil {
		t.Logf("pprof: WriteHeapProfile %s: %v", name, err)
		return
	}
	p.HeapProfile = heapPath
}

// printProfileSummary emits a human-readable table to test output.
// Sorted by latency descending so the slow tools stand out.
func printProfileSummary(t *testing.T, profiles []toolProfile) {
	t.Helper()
	sorted := make([]toolProfile, len(profiles))
	copy(sorted, profiles)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LatencyMS > sorted[j].LatencyMS })

	t.Log("E2E tool profile (sorted by latency desc):")
	t.Logf("  %-20s %8s %12s %10s  %-7s  %s",
		"tool", "ms", "alloc bytes", "allocs", "status", "notes")
	for _, p := range sorted {
		notes := ""
		switch p.Status {
		case "ok":
			notes = fmt.Sprintf("body=%d B", p.BodySize)
		default:
			notes = p.Reason
			if len(notes) > 60 {
				notes = notes[:60] + "..."
			}
		}
		t.Logf("  %-20s %8d %12d %10d  %-7s  %s",
			p.Name, p.LatencyMS, p.AllocBytes, p.AllocCount, p.Status, notes)
	}
}

// writeProfileManifest emits the per-tool JSON for downstream
// consumption (regression baseline, flamegraph linker, dashboards).
func writeProfileManifest(t *testing.T, path string, profiles []toolProfile) {
	t.Helper()
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(struct {
		GeneratedAt time.Time     `json:"generated_at"`
		GoVersion   string        `json:"go_version"`
		ToolCount   int           `json:"tool_count"`
		Profiles    []toolProfile `json:"profiles"`
	}{
		GeneratedAt: time.Now().UTC(),
		GoVersion:   runtime.Version(),
		ToolCount:   len(profiles),
		Profiles:    profiles,
	}))
	t.Logf("E2E profile manifest: %s", path)
}

// writeE2EFixture builds a multi-package Go fixture that exercises
// the cross-reference graph (auth, billing, util packages with calls
// between them) so every read-surface tool has something meaningful
// to surface. Returns the fixture root.
//
// Schema-projected paths produced (mache go-schema):
//   - auth/functions/Validate/source
//   - billing/functions/Charge/source  (calls Validate, util.Format)
//   - util/functions/Format/source
//   - main/functions/main/source       (calls Charge)
func writeE2EFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"go.mod": `module e2e_fixture

go 1.21
`,
		"auth/auth.go": `// Package auth provides Validate as the canonical credential check.
package auth

// Validate returns nil when the token is acceptable.
func Validate(token string) error {
	if token == "" {
		return errEmpty
	}
	return nil
}

var errEmpty = newErr("empty token")

func newErr(s string) error { return &authErr{msg: s} }

type authErr struct{ msg string }

func (e *authErr) Error() string { return e.msg }
`,
		"util/util.go": `// Package util provides Format for shared logging.
package util

import "strings"

// Format prefixes the message with [INFO] and lowercases it.
func Format(msg string) string {
	return "[INFO] " + strings.ToLower(msg)
}

// Helper is internal — exists to give find_callers a richer graph.
func Helper(s string) string { return Format(s) + "!" }
`,
		"billing/billing.go": `// Package billing handles charges; calls auth.Validate + util.Format.
package billing

import (
	"e2e_fixture/auth"
	"e2e_fixture/util"
)

// Charge runs validation then logs.
func Charge(token string, amount int) error {
	if err := auth.Validate(token); err != nil {
		return err
	}
	_ = util.Format("charged")
	return nil
}

// Refund mirrors Charge for fan-out shape.
func Refund(token string, amount int) error {
	if err := auth.Validate(token); err != nil {
		return err
	}
	_ = util.Format("refunded")
	return nil
}
`,
		"main.go": `// Package main is the entry point.
package main

import (
	"e2e_fixture/billing"
	"e2e_fixture/util"
)

func main() {
	_ = billing.Charge("token", 100)
	_ = util.Helper("done")
}
`,
	}

	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	return root
}

// makeRequest is the existing test helper from serve_test.go (same
// package). The harness reuses it rather than redeclaring; if that
// helper moves or changes signature, this test will fail loudly at
// compile time, which is the right signal.
