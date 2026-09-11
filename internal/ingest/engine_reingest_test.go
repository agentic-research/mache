package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reingestSchema is the smallest schema with the shape that matters: a shared
// `$` container holding one construct per function, each with a source leaf.
func reingestSchema() *api.Topology {
	return &api.Topology{Version: "1", Nodes: []api.Node{{
		Name:     "fns",
		Selector: "$",
		Children: []api.Node{{
			Name:     "{{.name}}",
			Selector: "(function_item name: (identifier) @name) @scope",
			Files:    []api.Leaf{{Name: "source", ContentTemplate: "{{.scope}}"}},
		}},
	}}}
}

// reingestEngine writes files into a temp dir, ingests it, and returns the
// store, the engine and the directory.
func reingestEngine(t *testing.T, files map[string]string) (*graph.MemoryStore, *Engine, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	store := graph.NewMemoryStore()
	engine := NewEngine(reingestSchema(), store)
	attachLeylineAST(t, engine, dir)
	require.NoError(t, engine.Ingest(dir))
	return store, engine, dir
}

// childrenOf is the sorted child ids of id, or nil when it does not exist.
func childrenOf(t *testing.T, store *graph.MemoryStore, id string) []string {
	t.Helper()
	kids, err := store.ListChildren(id)
	if err != nil {
		return nil
	}
	return kids
}

// TestReIngestFile_KeepsConstructIDs pins the no-op case: re-ingesting a file
// nobody edited must leave the graph exactly as it was.
//
// Before mache-399c25 it did not. claimedIDs was reset only in Ingest, so the
// file found its OWN previous id taken and suffixed around it — one no-op
// re-ingest turned [fns/alpha] into [fns/alpha, fns/alpha.from_lib_rs], and
// fns/alpha was left behind with no children because ReplaceFileNodes reaches
// only nodes with an Origin, which a construct directory has not.
func TestReIngestFile_KeepsConstructIDs(t *testing.T) {
	store, engine, dir := reingestEngine(t, map[string]string{"lib.rs": "pub fn alpha() {}\n"})
	rs := filepath.Join(dir, "lib.rs")

	before := childrenOf(t, store, "fns")
	require.Equal(t, []string{"fns/alpha"}, before)

	for i := range 2 {
		require.NoError(t, engine.ReIngestFile(rs))
		assert.Equal(t, before, childrenOf(t, store, "fns"),
			"re-ingest %d changed the graph of an unedited file", i+1)
		assert.Equal(t, []string{"fns/alpha/source"}, childrenOf(t, store, "fns/alpha"),
			"re-ingest %d left the construct without its source", i+1)
	}
}

// TestReIngestFile_RenameReplacesTheNode pins the edited case. A function
// renamed to beta must be projected as beta, and alpha must be gone.
//
// Before the fix it was projected as `alpha.from_lib_rs.2` — the token `beta`
// appeared nowhere in the graph, and two empty husks were left behind.
//
// The AST is re-parsed after the edit. ReIngestFile deliberately does not do
// that (the served `_ast` is frozen at startup, a disclosed gap tracked
// separately), so without re-attaching, the walker would still report `alpha`
// and this would be testing the frozen-AST gap rather than node identity.
func TestReIngestFile_RenameReplacesTheNode(t *testing.T) {
	store, engine, dir := reingestEngine(t, map[string]string{"lib.rs": "pub fn alpha() {}\n"})
	rs := filepath.Join(dir, "lib.rs")

	require.NoError(t, os.WriteFile(rs, []byte("pub fn beta() {}\n"), 0o644))
	attachLeylineAST(t, engine, dir)
	require.NoError(t, engine.ReIngestFile(rs))

	assert.Equal(t, []string{"fns/beta"}, childrenOf(t, store, "fns"),
		"the renamed function must replace the old node, not accumulate beside it")
	assert.Equal(t, []string{"fns/beta/source"}, childrenOf(t, store, "fns/beta"))

	_, err := store.GetNode("fns/alpha")
	assert.Error(t, err, "fns/alpha survived a rename")
	assert.Empty(t, store.LookupDef("alpha"), "the old definition token still resolves")
	assert.Equal(t, []string{"fns/beta"}, store.LookupDef("beta"))
}

// TestReIngestFile_LeavesOtherFilesAlone is the anti-weasel test. Releasing
// every claim, or deleting by container, passes both tests above and fails
// this one.
//
// a.rs and b.rs both declare `shared`, so one of them holds the bare id and
// the other a `.from_` suffix — a genuine cross-file collision that must
// SURVIVE a re-ingest of the other file.
func TestReIngestFile_LeavesOtherFilesAlone(t *testing.T) {
	store, engine, dir := reingestEngine(t, map[string]string{
		"a.rs": "pub fn shared() {}\npub fn only_a() {}\n",
		"b.rs": "pub fn shared() {}\n",
	})

	before := childrenOf(t, store, "fns")
	require.Len(t, before, 3, "expected two `shared` constructs and one `only_a`: %v", before)

	require.NoError(t, engine.ReIngestFile(filepath.Join(dir, "b.rs")))

	assert.ElementsMatch(t, before, childrenOf(t, store, "fns"),
		"re-ingesting b.rs changed which ids exist; a.rs's constructs or the shared container moved")
	assert.Equal(t, []string{"fns/only_a/source"}, childrenOf(t, store, "fns/only_a"),
		"a.rs's construct lost its source when b.rs was re-ingested")
}

// TestReIngestFile_LeavesNoEmptyConstructDirs states the husk invariant
// directly: after any re-ingest, every construct under the container still has
// its source. An empty construct directory is a node an agent can list and
// then read nothing from.
func TestReIngestFile_LeavesNoEmptyConstructDirs(t *testing.T) {
	store, engine, dir := reingestEngine(t, map[string]string{"lib.rs": "pub fn alpha() {}\n"})
	rs := filepath.Join(dir, "lib.rs")

	for _, body := range []string{"pub fn alpha() {}\n", "pub fn beta() {}\n", "pub fn gamma() {}\n"} {
		require.NoError(t, os.WriteFile(rs, []byte(body), 0o644))
		attachLeylineAST(t, engine, dir)
		require.NoError(t, engine.ReIngestFile(rs))
	}

	kids := childrenOf(t, store, "fns")
	require.Len(t, kids, 1, "three edits of one file left %d constructs: %v", len(kids), kids)
	for _, id := range kids {
		assert.NotEmpty(t, childrenOf(t, store, id), "%s is an empty husk", id)
	}
}
