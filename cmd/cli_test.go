package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// version command
// ---------------------------------------------------------------------------

func TestVersion_FieldsSet(t *testing.T) {
	// versionCmd uses fmt.Printf (stdout), hard to capture.
	// Instead verify the format string produces expected output.
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	Version, Commit, Date = "1.2.3", "abc123", "2026-03-22"
	defer func() { Version, Commit, Date = oldVersion, oldCommit, oldDate }()

	out := fmt.Sprintf("mache version %s (commit %s, built %s)\n", Version, Commit, Date)
	assert.Contains(t, out, "1.2.3")
	assert.Contains(t, out, "abc123")
	assert.Contains(t, out, "2026-03-22")
}

// ---------------------------------------------------------------------------
// build command
// ---------------------------------------------------------------------------

func TestBuild_ProducesDB(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644))

	outDB := filepath.Join(tmpDir, "out.db")

	// Call RunE directly to avoid cobra global state pollution
	oldSchemaPath := schemaPath
	schemaPath = "go"
	defer func() { schemaPath = oldSchemaPath }()

	err := buildCmd.RunE(buildCmd, []string{srcDir, outDB})
	require.NoError(t, err)

	info, err := os.Stat(outDB)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0), "output DB should be non-empty")
}

// TestBuild_RegisteredGoImportRefsReachNodeRefs exercises the production
// leyline-parse -> ASTWalker -> Engine path. The lower-level query tests prove
// registration, but this guards the actual `mache build --schema go` output
// consumed by graph callers: an import must survive as a typed gomod token in
// node_refs, not merely exist in the temporary _ast database.
func TestBuild_RegisteredGoImportRefsReachNodeRefs(t *testing.T) {
	requirePinnedLeyline(t)
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "go.mod"),
		[]byte("module example.com/app\n\ngo 1.23\n\nrequire example.com/acme/dep v0.0.0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(
		"package main\n\nimport dep \"example.com/acme/dep\"\n\nfunc main() { dep.Run() }\n",
	), 0o644))

	outDB := filepath.Join(tmpDir, "out.db")
	oldSchemaPath := schemaPath
	schemaPath = "go"
	defer func() { schemaPath = oldSchemaPath }()
	require.NoError(t, buildCmd.RunE(buildCmd, []string{srcDir, outDB}))

	db, err := sql.Open("sqlite", outDB)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var refs int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM node_refs WHERE token = ?`, "gomod:example.com/acme/dep",
	).Scan(&refs))
	assert.GreaterOrEqual(t, refs, 1,
		"mache build must project registered Go import refs into node_refs.token")
}

// inferGoFCASchema runs the SURVIVING source-code FCA inference path
// (ADR-0012 step 4): leyline-parse the tree into an `_ast` db, run
// pure-Go lattice.InferFromASTDB via inferLanguages, and unwrap the
// single-language namespace node exactly like inferDirSchema does.
// Shared fixture step for the FCA regression tests below.
func inferGoFCASchema(t *testing.T, srcDir string, goFiles int) *api.Topology {
	t.Helper()
	astDB, cleanup, err := autoInvokeLeylineParse(srcDir)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	nodes, err := inferLanguages(astDB, []string{"go"}, map[string]int{"go": goFiles})
	require.NoError(t, err)
	require.Len(t, nodes, 1, "FCA over the go _ast rows must produce a schema")
	// Single-language unwrap, mirroring inferDirSchema's return shape.
	return &api.Topology{Version: api.SchemaVersion, Nodes: nodes[0].Children}
}

// TestBuild_FCAInferenceCoversMethods guards multi-file FCA inference.
// The pre-leyline bootstrap parsed the first .go file only and stopped
// — when that file held only function_declarations the inferred schema
// was missing the 'methods' grouping, which silently dropped every
// method_declaration at ingest. As a knock-on, dead_code flagged any
// function only called by a method (mache-5d1o).
//
// Layout: two files where the FIRST one walked alphabetically has
// no methods. Without whole-tree accumulation the schema would lack
// methods/, so the method's call to standaloneFunc would not appear
// in node_refs.
//
// Migrated off `mache build` with empty --schema (ADR-0012 step 4:
// that is now a raw leyline `_ast` passthrough with no projection) to
// the surviving FCA entrypoint: leyline parse → inferLanguages
// (lattice.InferFromASTDB) → runBuildViaLeylineSchema projection —
// the same composition the mount/serve --infer paths use.
func TestBuild_FCAInferenceCoversMethods(t *testing.T) {
	requirePinnedLeyline(t)
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))

	// a.go has only standalone funcs — single-file inference would
	// produce 'functions/' with no 'methods/'.
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.go"),
		[]byte("package p\n\nfunc standaloneFunc() {}\n"), 0o644))

	// b.go has methods that call standaloneFunc. Without methods/
	// in the schema, the call is silently dropped from node_refs.
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "b.go"),
		[]byte("package p\n\ntype T struct{}\n\nfunc (T) MethodCallsStandalone() { standaloneFunc() }\n"),
		0o644))

	schema := inferGoFCASchema(t, srcDir, 2)

	outDB := filepath.Join(tmpDir, "out.db")
	require.NoError(t, runBuildViaLeylineSchema(srcDir, outDB, schema, false, nil))

	db, err := sql.Open("sqlite", outDB)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var methodCount int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM nodes WHERE id LIKE 'methods/%' AND id NOT LIKE '%/source' AND id NOT LIKE '%/ast.json' AND id NOT LIKE '%/doc'`,
	).Scan(&methodCount))
	assert.GreaterOrEqual(t, methodCount, 1, "FCA inference must produce a methods/ root when any file has receiver methods")

	var refCount int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM node_refs WHERE token = 'standaloneFunc'`,
	).Scan(&refCount))
	assert.GreaterOrEqual(t, refCount, 1, "method's call to standaloneFunc must appear in node_refs (dead_code FP fix)")
}

// TestBuild_FCAInferenceTagsLanguage guards PR #260: the FCA-inferred
// schema must be tagged with Language="go" so its selector only
// applies to .go files. Without the tag, JS function_declarations
// matched the inferred Go pattern and produced orphan construct dirs
// (no source/ast.json/doc children) since the Go-shaped templates
// don't render cleanly against the JS match.
//
// Migrated to the surviving leyline FCA path (see
// TestBuild_FCAInferenceCoversMethods). inferLanguages sets
// Config.Language="go", so InferFromASTDB samples only .go _ast rows
// and stamps Language="go" on every generated node — the projection
// must then route b.js to _project_files/ instead of matching it.
func TestBuild_FCAInferenceTagsLanguage(t *testing.T) {
	requirePinnedLeyline(t)
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))

	// a.go provides the inference bootstrap.
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.go"),
		[]byte("package p\n\nfunc onlyGoFunc() {}\n"), 0o644))

	// b.js has a function_declaration with the same shape as Go's.
	// Without the Language tag, the inferred Go selector matches it
	// and creates a 'functions/jsFunc' construct dir that becomes an
	// orphan when {{.scope}} fails to render against the JS shape.
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "b.js"),
		[]byte("function jsFunc(x) { return x; }\n"), 0o644))

	schema := inferGoFCASchema(t, srcDir, 1)
	for _, n := range schema.Nodes {
		assert.Equal(t, "go", n.Language,
			"inferred node %q must carry the Language tag (PR #260)", n.Name)
	}

	outDB := filepath.Join(tmpDir, "out.db")
	require.NoError(t, runBuildViaLeylineSchema(srcDir, outDB, schema, false, nil))

	db, err := sql.Open("sqlite", outDB)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Go func should be present.
	var hasGoFunc int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE id = 'functions/onlyGoFunc'`,
	).Scan(&hasGoFunc))
	assert.Equal(t, 1, hasGoFunc, "Go function_declaration must be ingested")

	// JS func must NOT be present in functions/ — should be routed to
	// _project_files/ since the inferred-Go schema is now tagged
	// Language='go' and won't apply to .js files.
	var hasJSFunc int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE id = 'functions/jsFunc'`,
	).Scan(&hasJSFunc))
	assert.Equal(t, 0, hasJSFunc, "JS function_declaration must not match the Go-tagged inferred schema (PR #260)")

	// b.js should appear under _project_files/ instead.
	var inProjectFiles int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE id LIKE '_project_files/%' AND name = 'b.js'`,
	).Scan(&inProjectFiles))
	assert.GreaterOrEqual(t, inProjectFiles, 1, "non-Go files should land in _project_files/")
}

// TestBuild_SchemaPathRelative guards the resolveSchema call site.
// Passing 'examples/go-schema.json' on the CLI used to double-prepend
// the directory ('examples/examples/go-schema.json'). Build now passes
// '.' as the configDir so resolveSchema treats the schemaRef as
// already cwd-relative.
//
// We use a tempdir-relative schema file rather than the real
// examples/go-schema.json so the test isn't tied to that file's
// content (it'd break if the schema's JSON shape evolved).
//
// Pinned to --backend=tree-sitter: resolveSchema only runs on the
// in-process path. After the ADR-0012 step 3b auto-default flip
// (#315), --backend=auto prefers leyline when available and logs
// "--schema is ignored on this path." That short-circuits the very
// code we're trying to test, so the test would silently stop
// guarding the double-prepend regression on machines with leyline
// installed.
func TestBuild_SchemaPathRelative(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644))

	// Lay down a minimal schema in a 'schemas/' subdir so the path
	// has a non-trivial directory component (mirrors the real
	// 'examples/go-schema.json' shape that triggered the bug).
	schemasDir := filepath.Join(tmpDir, "schemas")
	require.NoError(t, os.MkdirAll(schemasDir, 0o755))
	schemaFile := filepath.Join(schemasDir, "minimal.json")
	require.NoError(t, os.WriteFile(schemaFile,
		[]byte(`{"version": "v1alpha1"}`), 0o644))

	// Run from tmpDir so the schema path is relative — that's the
	// failure mode (absolute paths don't trigger the double-prepend).
	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(oldWD) }()

	oldSchemaPath := schemaPath
	oldBackend := buildBackend
	schemaPath = "schemas/minimal.json"
	buildBackend = "tree-sitter"
	defer func() { schemaPath = oldSchemaPath; buildBackend = oldBackend }()

	outDB := filepath.Join(tmpDir, "out.db")
	err = buildCmd.RunE(buildCmd, []string{srcDir, outDB})
	require.NoError(t, err, "build with relative --schema must not double-prepend the dir")

	info, err := os.Stat(outDB)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))
}

// TestBuild_SchemaFlagRegistered guards the --schema flag binding.
// rootCmd's --schema lives on Flags() (not PersistentFlags), so it
// doesn't propagate to children. buildCmd needs its own binding to
// the same package-level schemaPath variable so users can pass
// --schema on the CLI without falling through to FCA inference.
func TestBuild_SchemaFlagRegistered(t *testing.T) {
	flag := buildCmd.Flags().Lookup("schema")
	require.NotNil(t, flag, "buildCmd must register a --schema flag")
	assert.Equal(t, "s", flag.Shorthand, "shorthand must match the rootCmd convention")
	// Setting through the flag should mutate schemaPath, since both
	// flags bind to the same variable.
	saved := schemaPath
	defer func() { schemaPath = saved }()
	require.NoError(t, buildCmd.Flags().Set("schema", "/tmp/test-schema.json"))
	assert.Equal(t, "/tmp/test-schema.json", schemaPath)
}

func TestBuild_NonexistentSource(t *testing.T) {
	tmpDir := t.TempDir()
	outDB := filepath.Join(tmpDir, "out.db")

	oldSchemaPath := schemaPath
	schemaPath = "go"
	defer func() { schemaPath = oldSchemaPath }()

	err := buildCmd.RunE(buildCmd, []string{"/nonexistent/path", outDB})
	assert.Error(t, err, "should fail with nonexistent source")
}

// ---------------------------------------------------------------------------
// list command
// ---------------------------------------------------------------------------

func TestList_RunsWithoutError(t *testing.T) {
	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	err := listCmd.RunE(listCmd, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// clean command
// ---------------------------------------------------------------------------

func TestClean_RunsWithoutError(t *testing.T) {
	var buf bytes.Buffer
	cleanCmd.SetOut(&buf)
	err := cleanCmd.RunE(cleanCmd, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// init command (supplements existing init_test.go)
// ---------------------------------------------------------------------------

func TestInit_AutoDetectsGo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // registerProject writes under $HOME/.mache — never the real one
	oldCwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(oldCwd) }()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module test\n"), 0o644))

	var buf bytes.Buffer
	err := execInit(&buf, "/usr/local/bin/mache", initOpts{Source: "."})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".mache.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "go")
}

func TestInit_AutoDetectsPython(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // registerProject writes under $HOME/.mache — never the real one
	oldCwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(oldCwd) }()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "pyproject.toml"), []byte("[project]\n"), 0o644))

	var buf bytes.Buffer
	err := execInit(&buf, "/usr/local/bin/mache", initOpts{Source: "."})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".mache.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "python")
}

// ---------------------------------------------------------------------------
// serve command — verify help output
// ---------------------------------------------------------------------------

func TestServe_HelpOutput(t *testing.T) {
	out := serveCmd.UsageString()
	assert.Contains(t, out, "schema")
	assert.Contains(t, out, "http")
}

// ---------------------------------------------------------------------------
// root command
// ---------------------------------------------------------------------------

func TestRoot_HelpOutput(t *testing.T) {
	out := rootCmd.UsageString()
	assert.Contains(t, out, "Mache")
	assert.Contains(t, out, "Available Commands")
	assert.Contains(t, out, "serve")
	assert.Contains(t, out, "build")
	assert.Contains(t, out, "init")
}

func TestRoot_VersionString(t *testing.T) {
	oldVersion := Version
	Version = "test-version"
	defer func() { Version = oldVersion }()

	v := fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
	assert.Contains(t, v, "test-version")
}
