# Public Schema Projection and Nested Address Refs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make schema-projected builds a stable public Go API and preserve typed address references from nested Go and Terraform files.

**Architecture:** A new public `schema` package owns preset assets and reference resolution. The public `build` package owns the pinned-leyline-to-projected-SQLite pipeline, while the CLI delegates to it and retains only CLI provenance and warning behavior. Ingestion passes leyline's root-relative source ID unchanged to all AST queries.

**Tech Stack:** Go 1.25, modernc SQLite, pinned leyline v0.18.2 executable, Cobra, testify.

## Global Constraints

- The Go build remains CGO-free; Mache invokes the pinned leyline executable rather than linking Rust or C parsers.
- Preset JSON has one embedded owner; no copied preset registry or schema payloads.
- Tests must exercise public build or CLI production paths in addition to focused ingestion tests.
- Schema builds must fail instead of returning a hollow projection when an attributable schema language has source files but no parsed AST rows.
- Bead mutations use rsry MCP and the exported `.beads/beads.jsonl` remains current.

______________________________________________________________________

### Task 1: Public Schema Resolution

**Files:**

- Create: `schema/schema.go`
- Create: `schema/schema_test.go`
- Move: `cmd/schemas/*.json` to `schema/presets/*.json`
- Modify: `cmd/schemas.go`
- Modify: `cmd/config.go`
- Modify: `cmd/config_test.go`

**Interfaces:**

- Produces: `schema.Resolution`, `schema.ParseTopology`, `schema.Resolve`, `schema.LoadPreset`, and `schema.AvailablePresets`.

- Consumes: `api.Topology` and `internal/lang.Registry`.

- [ ] **Step 1: Write failing external-package schema tests**

```go
func TestResolvePreset(t *testing.T) {
    got, err := schema.Resolve("go", t.TempDir())
    require.NoError(t, err)
    require.NotEmpty(t, got.Topology.Nodes)
    require.Equal(t, []string{"go"}, got.Languages)
}

func TestResolveContainedFile(t *testing.T) {
    dir := t.TempDir()
    require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.json"), []byte(`{"version":"v1"}`), 0o644))
    got, err := schema.Resolve("custom.json", dir)
    require.NoError(t, err)
    require.Equal(t, "v1", got.Topology.Version)
}

func TestResolveRejectsEscape(t *testing.T) {
    _, err := schema.Resolve("../outside.json", t.TempDir())
    require.ErrorContains(t, err, "escapes")
}
```

- [ ] **Step 2: Run tests to verify the package/API is absent**

Run: `go test ./schema`
Expected: FAIL because `schema` and its exported API do not exist.

- [ ] **Step 3: Implement the schema owner and move preset assets**

```go
type Resolution struct {
    Topology  *api.Topology
    Languages []string
}

func ParseTopology(data []byte) (*api.Topology, error) {
    var topology api.Topology
    if err := json.Unmarshal(data, &topology); err != nil {
        return nil, fmt.Errorf("parse schema: %w", err)
    }
    return &topology, nil
}
```

Embed `schema/presets/*.json`, derive language preset paths from
`internal/lang.Registry`, add only the three data presets (`cli`, `mcp`, and
`mcp-registry`) explicitly, and return sorted names. Resolve relative paths
against `baseDir` after symlink-aware containment validation. Make the cmd
helpers thin delegates so existing serve/mount call sites retain their current
signatures while all preset bytes come from the public package.

- [ ] **Step 4: Run focused schema and configuration tests**

Run: `go test ./schema ./cmd -run 'TestResolveSchema|TestPreset|TestConfig'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add schema cmd/schemas.go cmd/config.go cmd/config_test.go
git commit -m "[mache-734971] feat(schema): expose preset and file resolution"
```

### Task 2: Public Schema Build Pipeline

**Files:**

- Modify: `build/build.go`
- Create: `build/schema.go`
- Modify: `build/build_test.go`
- Move: `cmd/build_leyline_coverage.go` logic to `build/schema_coverage.go`
- Modify: `cmd/build_leyline_coverage_test.go`

**Interfaces:**

- Consumes: `schema.Resolve`, `api.Topology`, public aliases from `ingest`.

- Produces: `build.ParseWithSchema(source, output string, topology *api.Topology) error` and `build.ParseWithSchemaRef(source, output, ref, baseDir string) error`.

- [ ] **Step 1: Write failing public-package projection tests**

```go
func TestParseWithSchemaProjectsCallerTopology(t *testing.T) {
    topology, err := schema.ParseTopology([]byte(`{
      "version":"v1",
      "nodes":[{"name":"functions","selector":"$","language":"go",
        "children":[{"name":"{{.name}}","selector":"(function_declaration name: (identifier) @name) @scope",
          "files":[{"name":"source","content_template":"{{.scope}}"}]}]}]
    }`))
    require.NoError(t, err)
    require.NoError(t, build.ParseWithSchema(sourceDir, outputDB, topology))
    require.Equal(t, 1, sqliteCount(t, outputDB, `SELECT count(*) FROM nodes WHERE id='functions/Use/source'`))
}
```

Also add a preset entry-point test that calls
`build.ParseWithSchemaRef(sourceDir, outputDB, "go", sourceDir)`.

- [ ] **Step 2: Run the public build tests and observe the missing API**

Run: `go test ./build -run 'TestParseWithSchema'`
Expected: FAIL with undefined `build.ParseWithSchema` and `build.ParseWithSchemaRef`.

- [ ] **Step 3: Implement the shared projection lifecycle**

```go
func ParseWithSchema(source, output string, topology *api.Topology) error {
    return parseWithSchema(source, output, topology, nil)
}

func ParseWithSchemaRef(source, output, ref, baseDir string) error {
    resolved, err := schema.Resolve(ref, baseDir)
    if err != nil {
        return fmt.Errorf("load schema: %w", err)
    }
    return parseWithSchema(source, output, resolved.Topology, resolved.Languages)
}
```

`parseWithSchema` resolves and records the pinned leyline binary, parses into a
temporary DB, removes WAL/SHM files on cleanup, opens and tunes the private read
connection, validates grammar coverage, removes/truncates the output, creates
the SQLite writer, wires `Engine` and `ASTWalker`, ingests, and returns writer
close failures. Ensure every early return closes both writer and AST DB.

- [ ] **Step 4: Move and retain grammar-coverage tests**

Move the pure coverage helpers into the `build` package and test language hints,
preset-supplied languages, absent source extensions, and missing `_ast` rows.
The public functions return the existing actionable no-grammar error.

- [ ] **Step 5: Run public build and coverage tests**

Run: `go test ./build`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add build cmd/build_leyline_coverage.go cmd/build_leyline_coverage_test.go
git commit -m "[mache-734971] feat(build): expose schema projection"
```

### Task 3: Root-Relative Address Refs

**Files:**

- Modify: `internal/ingest/ast_walker_extract.go`
- Modify: `internal/ingest/engine_treesitter.go`
- Modify: `internal/ingest/address_refs_test.go`
- Modify: `build/build_test.go`

**Interfaces:**

- Consumes: root-relative IDs from `Engine.sourceIDFor`.

- Produces: `ASTWalker.ExtractAddressRefs(sourceID, langName)` querying the exact leyline `_source.id`.

- [ ] **Step 1: Add failing nested Go and Terraform production regressions**

Create root and `sub/` source files. For Go, call
`build.ParseWithSchemaRef(..., "go", ...)` and assert both
`gomod:example.com/root/dep` and `gomod:example.com/nested/dep` occur in
`node_refs.token`; also query the corresponding `nodes.record` values to prove
the nested construct is not hollow. For Terraform, put a module block under
`infra/nested.tf` and assert `mod:./modules/vpc` reaches `node_refs`.

- [ ] **Step 2: Run regressions to prove the basename bug**

Run: `go test ./build -run 'TestParseWithSchemaRef_(NestedGoRefs|NestedTerraformRefs)' -count=1`
Expected: FAIL because root refs exist while nested refs are absent.

- [ ] **Step 3: Pass the exact source ID**

```go
func (w *ASTWalker) ExtractAddressRefs(sourceID, langName string) ([]string, error) {
    refs, err := w.fileAddrRefs(sourceID, langName)
    if err != nil {
        return nil, err
    }
    return w.dedupAddrTokens(refs, ""), nil
}
```

In `processSourceFileResult`, call
`w.ExtractAddressRefs(sourceID, result.job.langName)`. Remove the now-unused
`path/filepath` import and document that the method consumes `_source.id`, not
an arbitrary OS path.

- [ ] **Step 4: Add a focused walker assertion for nested IDs**

Use `parseSourceToASTWalker` with `sub/nested.go`, call
`ExtractAddressRefs("sub/nested.go", "go")`, and assert the nested `gomod:`
token. This pins the API invariant independently of engine orchestration.

- [ ] **Step 5: Run ingestion and build suites**

Run: `go test ./internal/ingest ./build -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/ast_walker_extract.go internal/ingest/engine_treesitter.go internal/ingest/address_refs_test.go build/build_test.go
git commit -m "[mache-498bc3] fix(ingest): preserve nested address refs"
```

### Task 4: CLI Delegation and Parity

**Files:**

- Modify: `cmd/build.go`
- Modify: `cmd/cli_test.go`
- Modify: `ingest/ingest.go`

**Interfaces:**

- Consumes: `build.ParseWithSchemaRef` and `build.ParseWithSchema`.

- Produces: unchanged `mache build --schema REF` behavior with no private projection recipe.

- [ ] **Step 1: Add a failing CLI nested-ref regression**

Drive `runBuildViaLeyline` with `schemaPath = "go"` over root and nested Go
files, open the output database, and assert both `gomod:` tokens. The test must
not call `parseSourceToASTWalker` or construct `Engine` itself.

- [ ] **Step 2: Refactor the CLI to delegate**

Replace the temp DB / DB tuning / engine / writer block in
`runBuildViaLeylineSchema` with the shared public build call. Keep CLI logging,
`writeBuildMetadata`, and `warnIfEmptyBuild` after success. Make
`runBuildViaLeyline` use `build.ParseWithSchemaRef` for a schema reference and
remove imports made obsolete by delegation.

- [ ] **Step 3: Correct the PR #621 public comments**

Document that `NewASTWalker` constructs a walker over a caller-owned open
`*sql.DB`, and remove the stale implementation note now satisfied by the public
build API.

- [ ] **Step 4: Run CLI parity tests**

Run: `go test ./cmd -run 'TestBuild|TestLeylineSchemaCoverage' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/build.go cmd/cli_test.go ingest/ingest.go
git commit -m "[mache-734971] refactor(cmd): share public schema build path"
```

### Task 5: Documentation, Beads, and Release Gates

**Files:**

- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `CHANGELOG.md`
- Modify: `.beads/beads.jsonl`

**Interfaces:**

- Consumes: the final public APIs and verified behavior.

- Produces: discoverable usage and a reviewable, pushed PR branch.

- [ ] **Step 1: Document the public API and boundary**

Add an example that resolves a preset or parses a topology and calls the public
build package. State that leyline is the sole parser subprocess, Mache owns
schema projection and registered address refs, and source IDs remain
root-relative across the boundary.

- [ ] **Step 2: Add changelog entries**

Record the public schema build API and the nested `gomod:` / Terraform ref fix
under the current unreleased section, without claiming a new leyline version or
LLO-side change.

- [ ] **Step 3: Run formatting and focused verification**

Run: `gofumpt -w schema build cmd internal/ingest ingest`
Run: `go test ./schema ./build ./internal/ingest ./cmd -count=1`
Run: `git diff --check`
Expected: all commands pass.

- [ ] **Step 4: Run repository gates and installation validation**

Run: `task check`
Run: `task build`
Run: `task install`
Run: `mache version`
Expected: all gates pass and the installed binary reports the repository's
current version with the pinned leyline version unchanged at v0.18.2.

- [ ] **Step 5: Update and close beads through rsry MCP**

Comment verification evidence on `mache-498bc3` and `mache-734971`, close both
only after all acceptance criteria pass, and refresh `.beads/beads.jsonl` via
the MCP-backed export workflow.

- [ ] **Step 6: Commit and publish**

```bash
git add README.md docs/ARCHITECTURE.md CHANGELOG.md .beads/beads.jsonl
git commit -m "[mache-734971] docs: publish schema build API"
git pull --rebase origin main
git push --force-with-lease origin fix/three-correctness-beads
```

Verify: `git status --short --branch` shows the branch up to date with its
remote, then open the PR and enable auto-merge after required checks are green.
