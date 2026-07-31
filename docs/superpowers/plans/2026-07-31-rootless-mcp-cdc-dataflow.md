# Rootless MCP, CDC, and reference-flow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make rootless Codex stdio Mache usable, expose opt-in CDC for the managed Leyline daemon, and add a bounded reference-flow MCP query.

**Architecture:** A rootless stdio process may use one explicit positional source; a shared HTTP daemon retains the safe error. `serve --cdc` is forwarded only to Mache's managed Leyline daemon. `get_dataflow` performs deterministic bounded traversal through existing caller/callee operations and labels every edge `node_ref`.

**Tech Stack:** Go, Cobra, mcp-go, testify, Leyline UDS, Markdown.

## Global Constraints

- Never infer a project root for a shared HTTP daemon.
- CDC is opt-in; pre-built databases and external control daemons remain operator-managed.
- The first flow tool is reference flow, not LSP-confirmed binding, SSA, taint, or data dependence.
- Regenerate `server.json` after adding the MCP tool.

______________________________________________________________________

### Task 1: Rootless stdio fallback

**Files:**

- Modify: `cmd/serve_registry.go`
- Modify: `cmd/serve_test.go`
- Modify: `GETTING-STARTED.md`
- Test: `cmd/docs_cli_surface_test.go`

**Interfaces:**

- Produces: `fallbackGraphForSession` uses exactly one positional source only when `serveStdio` is true.

- [ ] **Step 1: Write the failing test**

```go
func TestGraphRegistry_RootDiscoveryFailureWithExplicitStdioSourceUsesSource(t *testing.T) {
    prev := serveStdio
    serveStdio = true
    t.Cleanup(func() { serveStdio = prev })
    source := t.TempDir()
    registry := newGraphRegistry("", []string{source})
    g := registry.resolveSession(context.Background(), &failingRootsSession{id: "rootless-stdio"})
    assert.Equal(t, source, g.basePath)
}
```

- [ ] **Step 2: Run RED**

Run: `go test ./cmd -run TestGraphRegistry_RootDiscoveryFailureWithExplicitStdioSourceUsesSource -count=1`

Expected: FAIL because rootless fallback returns an error graph.

- [ ] **Step 3: Implement the minimum safe branch**

```go
if serveStdio && len(r.args) == 1 {
    source, err := filepath.Abs(r.args[0])
    if err == nil {
        r.registerSession(sessionID, source)
        return r.getOrCreateGraph(source)
    }
}
```

Place it after explicit `basePath` handling; preserve the existing error graph for HTTP, zero arguments, and multiple positional sources.

- [ ] **Step 4: Correct docs and run GREEN**

Use `mache serve --stdio --path .`; use `mache serve --stdio --path . ./code.db` for snapshots; explain root discovery happens before scanning.

Run: `go test ./cmd -run 'TestGraphRegistry_RootDiscoveryFailure|TestDocsCLISurface' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/serve_registry.go cmd/serve_test.go GETTING-STARTED.md cmd/docs_cli_surface_test.go
git commit -m "[mache-76c919] fix(mcp): support rootless stdio source"
```

### Task 2: Opt-in managed-daemon CDC

**Files:**

- Modify: `cmd/serve.go`
- Modify: `internal/leyline/daemon_source.go`
- Modify: `internal/leyline/socket.go`
- Test: `internal/leyline/socket_test.go`
- Modify: `GETTING-STARTED.md`

**Interfaces:**

- Produces: `SetDaemonCDC(bool)`, `DaemonCDC() bool`, and `mache serve --cdc`.

- [ ] **Step 1: Write the failing daemon argv test**

```go
func TestDiscoverOrStart_CDCAddsDaemonFlag(t *testing.T) {
    SetDaemonCDC(true)
    t.Cleanup(func() { SetDaemonCDC(false) })
    // Existing fake-leyline seam records argv.
    // Assert the recorded argv contains "--cdc".
}
```

- [ ] **Step 2: Run RED**

Run: `go test ./internal/leyline -run TestDiscoverOrStart_CDCAddsDaemonFlag -count=1`

Expected: FAIL because no CDC daemon argument exists.

- [ ] **Step 3: Implement and document**

```go
serveCmd.Flags().BoolVar(&serveCDC, "cdc", false, "Enable CDC in Mache's managed Leyline daemon")
leyline.SetDaemonCDC(serveCDC)
if DaemonCDC() { daemonArgs = append(daemonArgs, "--cdc") }
```

Document CDC-off and CDC-on stdio commands; require external `--control` daemons to use Leyline's own `--cdc`.

- [ ] **Step 4: Run GREEN and commit**

Run: `go test ./internal/leyline ./cmd -run 'TestDiscoverOrStart_CDCAddsDaemonFlag|TestServe' -count=1`

Expected: PASS.

```bash
git add cmd/serve.go internal/leyline/daemon_source.go internal/leyline/socket.go internal/leyline/socket_test.go GETTING-STARTED.md
git commit -m "[mache-76c919] feat(leyline): add opt-in managed CDC"
```

### Task 3: Bounded reference-flow MCP tool

**Files:**

- Create: `cmd/serve_dataflow.go`
- Modify: `cmd/serve_handlers.go`
- Modify: `cmd/serve_test.go`
- Modify: `cmd/all_tools_e2e_test.go`
- Modify: `server.json`

**Interfaces:**

- Produces: `makeGetDataflowHandler(graph.Graph) server.ToolHandlerFunc`.

- Input: required `symbol`; optional `direction=callers|callees|both`; optional `depth=1..5`.

- Output: `{symbol, roots, nodes:[{path,depth}], edges:[{from,to,direction,evidence:"node_ref"}], truncated}`.

- [ ] **Step 1: Write the failing handler test**

```go
func TestGetDataflow_BuildsBoundedNodeRefEdges(t *testing.T) {
    result, err := makeGetDataflowHandler(buildTestGraph(t))(context.Background(),
        makeRequest(map[string]any{"symbol": "Helper", "direction": "callees", "depth": 2}))
    require.NoError(t, err)
    assert.Contains(t, resultText(t, result), `"evidence":"node_ref"`)
}
```

- [ ] **Step 2: Run RED**

Run: `go test ./cmd -run TestGetDataflow_BuildsBoundedNodeRefEdges -count=1`

Expected: FAIL because the handler does not exist.

- [ ] **Step 3: Implement deterministic traversal**

Resolve roots through `defsMapProvider.DefsMap`, then traverse `GetCallers(filepath.Base(id))` and `GetCallees(id)`. Sort roots, neighbor IDs, nodes, and edges before marshaling. Cap at 500 nodes and set `truncated`; label every edge `node_ref`.

- [ ] **Step 4: Register, generate, and verify**

```go
s.AddTool(mcp.NewTool("get_dataflow", readTool(),
    mcp.WithString("symbol", mcp.Required()),
    mcp.WithString("direction"),
    mcp.WithNumber("depth"),
), r.wrapHandler(makeGetDataflowHandler))
```

Add valid arguments to the all-tools matrix, then run:

`go test ./cmd -run 'TestGetDataflow|TestE2E_AllMCPTools|TestToolRegistry' -count=1 && task gen:server-json && task gen:server-json:check`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/serve_dataflow.go cmd/serve_handlers.go cmd/serve_test.go cmd/all_tools_e2e_test.go server.json
git commit -m "[mache-76c919] feat(mcp): expose bounded reference flow"
```

### Task 4: CDC ablation verification and PR

**Files:**

- Modify: `GETTING-STARTED.md`

- Modify: `docs/superpowers/specs/2026-07-31-rootless-mcp-cdc-design.md`

- [ ] **Step 1: Document the ablation**

Run the same `get_dataflow` query with `mache serve --stdio --path .` and `mache serve --stdio --path . --cdc`. Compare the deterministic `get_dataflow` result before comparing latency; inspect snapshot generation/root separately through the existing `get_sheaf_status` tool. A changed graph result is a correctness regression.

- [ ] **Step 2: Verify, commit, and publish**

Run: `go test ./cmd ./internal/leyline -count=1 && task gen:server-json:check && git diff --check`

Expected: PASS.

```bash
git add GETTING-STARTED.md docs/superpowers/specs/2026-07-31-rootless-mcp-cdc-design.md
git commit -m "[mache-76c919] docs: document CDC reference-flow ablation"
git push -u origin fix/mache-76c919
gh pr create --base main --title "[mache-76c919] fix rootless MCP and add CDC flow ablation" --fill
```
