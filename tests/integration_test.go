package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/linter"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/nfsmount"
	"github.com/agentic-research/mache/internal/writeback"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFixture bundles the shared state for integration tests:
// a real Go source file, a schema, a MemoryStore graph,
// and a GraphFS wired with the real write-back pipeline.
type testFixture struct {
	srcDir  string
	srcFile string
	store   *graph.MemoryStore
	gfs     *nfsmount.GraphFS
}

const testGoSource = `package main

import "fmt"

const Version = "1.0"

func HelloWorld() {
	fmt.Println("Original Content Long Long Long")
}
`

// goFunctionsSchema projects Go functions as functions/<name>/source —
// the same layout the FCA-inferred schema used to produce for this fixture.
// Hand-written since mache-73b885 removed this package's direct tree-sitter
// fixture parsing (inference itself is covered by internal/lattice and the
// cmd infer tests).
func goFunctionsSchema() *api.Topology {
	return &api.Topology{
		Version: "v1",
		Nodes: []api.Node{{
			Name:     "functions",
			Selector: "$",
			Language: "go",
			Children: []api.Node{{
				Name:     "{{.name}}",
				Selector: "(function_declaration name: (identifier) @name) @scope",
				Files: []api.Leaf{{
					Name:            "source",
					ContentTemplate: "{{.scope}}",
				}},
			}},
		}},
	}
}

// setup creates a temp dir with a Go source file, ingests it into a
// MemoryStore under goFunctionsSchema, and wires up the real write-back
// callback on a GraphFS instance.
func setup(t *testing.T) *testFixture {
	t.Helper()

	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "main.go")
	require.NoError(t, os.WriteFile(srcFile, []byte(testGoSource), 0o644))

	schema := goFunctionsSchema()

	// Ingest into MemoryStore
	store := graph.NewMemoryStore()
	engine := ingest.NewEngine(schema, store)
	require.NoError(t, engine.Ingest(srcDir), "ingestion failed")

	// Wire GraphFS with real write-back
	gfs := nfsmount.NewGraphFS(store, schema)
	gfs.SetWriteBack(realWriteBack(store))

	return &testFixture{
		srcDir:  srcDir,
		srcFile: srcFile,
		store:   store,
		gfs:     gfs,
	}
}

// requireWriteDaemon gates tests that exercise the write-back pipeline: since
// mache-73b885, validation + lint run through the leyline daemon's validate
// op. A PRIVATE pinned daemon (never downloaded; skip when absent) is spawned
// and exposed via LEYLINE_SOCKET so the pipeline under test finds it.
func requireWriteDaemon(t *testing.T) {
	t.Helper()
	sock := lltest.StartPinnedDaemon(t)
	t.Setenv("LEYLINE_SOCKET", sock)
}

// realWriteBack replicates the cmd/mount_nfs.go NFS write-back pipeline:
// validate(+emit_ast) → format (gofumpt) → lint (same parse) → splice →
// surgical update.
func realWriteBack(store *graph.MemoryStore) func(string, graph.SourceOrigin, []byte) error {
	return func(nodeID string, origin graph.SourceOrigin, content []byte) error {
		node, err := store.GetNode(nodeID)
		if err != nil {
			return fmt.Errorf("node not found: %w", err)
		}

		// 1. Validate syntax — ONE leyline parse also yields the AST rows
		// the linter consumes below.
		astPayload, err := writeback.ValidateWithAST(content, origin.FilePath)
		if err != nil {
			store.WriteStatus.Store(filepath.Dir(nodeID), err.Error())
			draft := make([]byte, len(content))
			copy(draft, content)
			node.DraftData = draft
			return nil
		}

		// 2. Format (gofumpt for Go, hclwrite for HCL)
		formatted := writeback.FormatBuffer(content, origin.FilePath)

		// 3. Lint (warning only) — reuses step 1's parse
		if strings.HasSuffix(origin.FilePath, ".go") {
			if diags, err := linter.LintAST(astPayload); err == nil && len(diags) > 0 {
				var sb strings.Builder
				for _, d := range diags {
					sb.WriteString(d.String() + "\n")
				}
				store.WriteStatus.Store(filepath.Dir(nodeID)+"/lint", sb.String())
			} else {
				store.WriteStatus.Delete(filepath.Dir(nodeID) + "/lint")
			}
		}

		// 4. Splice into source file
		oldLen := origin.EndByte - origin.StartByte
		if err := writeback.Splice(origin, formatted); err != nil {
			return err
		}

		// 5. Surgical node update
		newOrigin := &graph.SourceOrigin{
			FilePath:  origin.FilePath,
			StartByte: origin.StartByte,
			EndByte:   origin.StartByte + uint32(len(formatted)),
		}
		delta := int32(len(formatted)) - int32(oldLen)
		if delta != 0 {
			store.ShiftOrigins(origin.FilePath, origin.EndByte, delta)
		}
		_ = store.UpdateNodeContent(nodeID, formatted, newOrigin, time.Now())
		store.WriteStatus.Store(filepath.Dir(nodeID), "ok")
		node.DraftData = nil

		store.Invalidate(nodeID)
		return nil
	}
}

// writeToNode opens a GraphFS file for writing, writes content, and closes it.
func writeToNode(t *testing.T, gfs *nfsmount.GraphFS, path string, content []byte) {
	t.Helper()
	f, err := gfs.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err, "open %s for write", path)
	_, err = f.Write(content)
	require.NoError(t, err, "write to %s", path)
	require.NoError(t, f.Close(), "close %s", path)
}

// readNode opens a GraphFS file, reads its content, and returns it as a string.
func readNode(t *testing.T, gfs *nfsmount.GraphFS, path string) string {
	t.Helper()
	f, err := gfs.Open(path)
	require.NoError(t, err, "open %s for read", path)
	defer func() { _ = f.Close() }()
	buf := make([]byte, 16384)
	n, _ := f.Read(buf)
	return string(buf[:n])
}

func TestIntegration_IngestAndProject(t *testing.T) {
	fix := setup(t)

	// The schema + ingestion should produce a HelloWorld node
	node, err := fix.store.GetNode("functions/HelloWorld/source")
	require.NoError(t, err, "functions/HelloWorld/source should exist in graph")
	assert.Contains(t, string(node.Data), "Original Content Long Long Long")
}

func TestIntegration_ContextAwareness(t *testing.T) {
	fix := setup(t)

	content := readNode(t, fix.gfs, "/functions/HelloWorld/context")
	assert.Contains(t, content, `import "fmt"`,
		"context virtual file should contain imports from the source file")
}

func TestIntegration_Truncation(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// Write shorter content — old tail must not remain in source file
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte("func HelloWorld() {\n\tfmt.Println(\"Short\")\n}\n"))

	src, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.NotContains(t, string(src), "Long",
		"old content tail should be truncated from source file")
	assert.Contains(t, string(src), "Short",
		"new content should appear in source file")
}

func TestIntegration_Diagnostics(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// Write broken syntax
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte("func HelloWorld() { BROKEN SYNTAX "))

	// Source file should NOT have the broken content (draft saved, not spliced)
	src, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.NotContains(t, string(src), "BROKEN",
		"broken syntax should not be spliced into source")

	// Diagnostics virtual file should report the error
	diag := readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/last-write-status")
	assert.Contains(t, diag, "syntax error",
		"diagnostics should report syntax error")

	// ast-errors should also have content
	astErrs := readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/ast-errors")
	assert.NotEmpty(t, astErrs, "ast-errors should have content after bad write")
}

func TestIntegration_Recovery(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// First: write broken syntax to set diagnostic state
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte("func HelloWorld() { BROKEN "))

	// Then: write valid code — should recover
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte("func HelloWorld() {\n\tfmt.Println(\"Fixed\")\n}\n"))

	src, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.Contains(t, string(src), "Fixed",
		"source file should contain recovered content")

	// Diagnostics should clear to "ok"
	diag := readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/last-write-status")
	assert.Contains(t, diag, "ok",
		"diagnostics should clear after valid write")
}

func TestIntegration_SequentialWrites(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// Two sequential writes to the same node — both should land.
	// The first write updates the origin via ShiftOrigins + UpdateNodeContent,
	// so the second write should pick up the new byte range.
	first := "func HelloWorld() {\n\tfmt.Println(\"First\")\n}\n"
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte(first))

	src1, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.Contains(t, string(src1), "First")

	second := "func HelloWorld() {\n\tfmt.Println(\"Second\")\n}\n"
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte(second))

	src2, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.Contains(t, string(src2), "Second")
	assert.NotContains(t, string(src2), "First",
		"first write should be replaced by second")
}

func TestIntegration_GofumptFormat(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// FormatGoBuffer uses format.Source which requires a complete Go file.
	// Function-body snippets (what NFS write-back sends) pass through unmodified.
	// Verify the pipeline still splices correctly even though formatting is a no-op.
	snippet := "func HelloWorld() {\n\tfmt.Println(\"formatted\")\n}\n"
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte(snippet))

	src, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)

	srcStr := string(src)
	assert.Contains(t, srcStr, "formatted",
		"content should be spliced into source")

	// Write status should be "ok" (successful pipeline completion)
	diag := readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/last-write-status")
	assert.Contains(t, diag, "ok",
		"write-back pipeline should complete successfully")
}

func TestIntegration_LintDiagnostic_NilSlice(t *testing.T) {
	requireWriteDaemon(t)
	fix := setup(t)

	// A valid write containing an uninitialized slice declaration must
	// splice AND surface the warning-only lint diagnostic — produced from
	// the SAME leyline parse that validated the write.
	snippet := "func HelloWorld() {\n\tvar s []int\n\t_ = s\n}\n"
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source", []byte(snippet))

	src, err := os.ReadFile(fix.srcFile)
	require.NoError(t, err)
	assert.Contains(t, string(src), "var s []int",
		"lint findings are warnings — the write must still splice")

	lint := readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/lint")
	assert.Contains(t, lint, "Nil slice declaration",
		"lint diagnostic should surface via _diagnostics/lint")
	assert.Contains(t, lint, "line 2:",
		"diagnostic line is 1-based in String() (var s []int is snippet line 2)")

	// A clean follow-up write clears the lint diagnostic.
	writeToNode(t, fix.gfs, "/functions/HelloWorld/source",
		[]byte("func HelloWorld() {\n\tfmt.Println(\"clean\")\n}\n"))
	lint = readNode(t, fix.gfs, "/functions/HelloWorld/_diagnostics/lint")
	assert.Contains(t, lint, "clean",
		"lint diagnostic should reset after a clean write")
}
