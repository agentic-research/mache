package graph

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildAuthRepo seeds an "auth" repo with one function (Validate) and
// a caller of itself. Used by the cross-repo tests.
func buildAuthRepo(t *testing.T) *MemoryStore {
	t.Helper()
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	store.AddNode(&Node{
		ID:       "functions/Validate",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Validate/source"},
	})
	store.AddNode(&Node{
		ID:   "functions/Validate/source",
		Mode: 0,
		Data: []byte("func Validate() {}"),
	})
	require.NoError(t, store.AddRef("Validate", "functions/Validate/source"))
	return store
}

// buildBillingRepo seeds a "billing" repo that ALSO calls Validate
// (cross-repo reference: billing/Charge calls auth/Validate). Used to
// verify GetCallers federation aggregates across repos.
func buildBillingRepo(t *testing.T) *MemoryStore {
	t.Helper()
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "functions", Mode: fs.ModeDir})
	store.AddNode(&Node{
		ID:       "functions/Charge",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Charge/source"},
	})
	store.AddNode(&Node{
		ID:   "functions/Charge/source",
		Mode: 0,
		Data: []byte("func Charge() { Validate() }"),
	})
	require.NoError(t, store.AddRef("Validate", "functions/Charge/source"))
	require.NoError(t, store.AddRef("Charge", "functions/Charge/source"))
	return store
}

// TestMultiRepoGraph_VirtualRootListsRepos asserts the synthesized
// root returns the registered repo names as children.
func TestMultiRepoGraph_VirtualRootListsRepos(t *testing.T) {
	m := NewMultiRepoGraph(map[string]Graph{
		"auth":    buildAuthRepo(t),
		"billing": buildBillingRepo(t),
	})

	root, err := m.GetNode("")
	require.NoError(t, err)
	assert.True(t, root.Mode.IsDir(), "virtual root must be a directory")
	assert.Equal(t, []string{"auth", "billing"}, root.Children, "deterministic alphabetical order")

	children, err := m.ListChildren("")
	require.NoError(t, err)
	assert.Equal(t, []string{"auth", "billing"}, children)

	stats, err := m.ListChildStats("")
	require.NoError(t, err)
	require.Len(t, stats, 2)
	for _, s := range stats {
		assert.True(t, s.IsDir, "repo top-level entries are directories")
	}
}

// TestMultiRepoGraph_GetNodeRoutesByPrefix asserts that "repo/path"
// IDs route to the right backend and the returned IDs are rewritten
// to keep the prefix.
func TestMultiRepoGraph_GetNodeRoutesByPrefix(t *testing.T) {
	auth := buildAuthRepo(t)
	billing := buildBillingRepo(t)
	m := NewMultiRepoGraph(map[string]Graph{"auth": auth, "billing": billing})

	// Node inside auth.
	n, err := m.GetNode("auth/functions/Validate/source")
	require.NoError(t, err)
	assert.Equal(t, "auth/functions/Validate/source", n.ID, "ID must keep the repo prefix")
	assert.Equal(t, []byte("func Validate() {}"), n.Data)

	// Directory inside billing — children are rewritten with prefix.
	n, err = m.GetNode("billing/functions/Charge")
	require.NoError(t, err)
	require.Len(t, n.Children, 1)
	assert.Equal(t, "billing/functions/Charge/source", n.Children[0],
		"children must carry the repo prefix so callers can navigate back")
}

// TestMultiRepoGraph_UnknownRepoReturnsNotFound asserts that a path
// pointing at a repo that wasn't registered surfaces ErrNotFound
// rather than a generic error or a routing-table panic.
func TestMultiRepoGraph_UnknownRepoReturnsNotFound(t *testing.T) {
	m := NewMultiRepoGraph(map[string]Graph{"auth": buildAuthRepo(t)})

	_, err := m.GetNode("nonexistent/foo/bar")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = m.ListChildren("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestMultiRepoGraph_GetCallersFederates is the headline cross-repo
// behavior: a token defined in one repo and referenced from multiple
// repos returns callers from every repo, with IDs prefixed.
func TestMultiRepoGraph_GetCallersFederates(t *testing.T) {
	auth := buildAuthRepo(t)
	billing := buildBillingRepo(t)
	m := NewMultiRepoGraph(map[string]Graph{"auth": auth, "billing": billing})

	callers, err := m.GetCallers("Validate")
	require.NoError(t, err)
	require.Len(t, callers, 2, "auth references Validate (self-ref test fixture); billing/Charge calls Validate")

	gotIDs := make([]string, len(callers))
	for i, c := range callers {
		gotIDs[i] = c.ID
	}
	assert.ElementsMatch(t,
		[]string{
			"auth/functions/Validate/source",
			"billing/functions/Charge/source",
		},
		gotIDs,
		"callers must carry repo prefix so callers can route back",
	)
}

// TestMultiRepoGraph_GetCallersNoMatchAnywhere asserts an unknown
// token returns no callers and no error (consistent with single-repo
// MemoryStore.GetCallers).
func TestMultiRepoGraph_GetCallersNoMatchAnywhere(t *testing.T) {
	m := NewMultiRepoGraph(map[string]Graph{
		"auth":    buildAuthRepo(t),
		"billing": buildBillingRepo(t),
	})

	callers, err := m.GetCallers("DefinitelyNotAToken")
	require.NoError(t, err)
	assert.Empty(t, callers)
}

// TestMultiRepoGraph_ReposListsRegisteredNames asserts the helper
// surface returns the same deterministic ordering as the virtual
// root's children.
func TestMultiRepoGraph_ReposListsRegisteredNames(t *testing.T) {
	m := NewMultiRepoGraph(map[string]Graph{
		"zebra":  NewMemoryStore(),
		"apple":  NewMemoryStore(),
		"middle": NewMemoryStore(),
	})

	assert.Equal(t, []string{"apple", "middle", "zebra"}, m.Repos(),
		"Repos() must return alphabetically-sorted names matching virtual-root children")
}

// TestMultiRepoGraph_RepoNameWithSlashPanics guards the namespace
// invariant: repo names cannot contain '/' because that's the
// separator. Caught at construction so misuse is loud.
func TestMultiRepoGraph_RepoNameWithSlashPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewMultiRepoGraph(map[string]Graph{"bad/name": NewMemoryStore()})
	})
}
