package resolve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/require"
)

// newFakeModule creates a self-contained temp Go module with one package
// so tests resolve entirely offline — no network, no dependency on the
// real GOPATH/module cache, and no assumption about what's importable from
// wherever `go test` happens to run.
func newFakeModule(t *testing.T) (workDir, importPath string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/fakemod\n\ngo 1.26\n"), 0o644))

	pkgDir := filepath.Join(dir, "greet")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "greet.go"),
		[]byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644))

	return dir, "example.com/fakemod/greet"
}

func TestGoModResolver_ResolvesToQueryableGraph(t *testing.T) {
	workDir, importPath := newFakeModule(t)
	r := &GoModResolver{WorkDir: workDir}

	g, err := r.Resolve(context.Background(), importPath)
	require.NoError(t, err)
	require.NotNil(t, g)

	// The resolved graph must be the ACTUAL package content — not just "no
	// error" — so this asserts against a symbol only this fixture package
	// defines, via the LookupDef fast path any SQLiteGraph-backed Resolve
	// result should support (see mache-04972b, this session — the whole
	// point of routing through graph.Build+graph.Open instead of a
	// hand-rolled import).
	ld, ok := g.(graph.DefsLookuper)
	require.True(t, ok, "resolved graph must satisfy graph.DefsLookuper")
	require.NotEmpty(t, ld.LookupDef("Hello"), "must find the function actually defined in the resolved module")
}

func TestGoModResolver_UnknownImportPathIsNotResolvable(t *testing.T) {
	workDir, _ := newFakeModule(t)
	r := &GoModResolver{WorkDir: workDir}

	_, err := r.Resolve(context.Background(), "example.com/fakemod/does-not-exist")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotResolvable)
}

func TestGoModResolver_CachesRepeatedResolution(t *testing.T) {
	workDir, importPath := newFakeModule(t)
	r := &GoModResolver{WorkDir: workDir}

	assertResolveIsCached(t, func() (graph.Graph, error) {
		return r.Resolve(context.Background(), importPath)
	})
}

func TestGoModResolver_MissingWorkDirErrors(t *testing.T) {
	r := &GoModResolver{}
	_, err := r.Resolve(context.Background(), "example.com/whatever")
	require.Error(t, err)
}

// TestGoModResolver_ViaRegistry proves the registry-integration story end
// to end: register the resolver under the "gomod" scheme ADR-0016 names,
// resolve through Registry.Resolve exactly as an MCP handler would.
func TestGoModResolver_ViaRegistry(t *testing.T) {
	workDir, importPath := newFakeModule(t)
	reg := NewRegistry()
	reg.Register("gomod", &GoModResolver{WorkDir: workDir})

	g, err := reg.Resolve(context.Background(), "gomod", importPath)
	require.NoError(t, err)
	require.NotNil(t, g)
}
