package resolve

import (
	"context"
	"errors"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResolver is a minimal Resolver for routing tests — it neither builds
// nor opens anything, just proves the Registry dispatched to it.
type fakeResolver struct {
	graph graph.Graph
	err   error
}

func (f *fakeResolver) Resolve(context.Context, string) (graph.Graph, error) {
	return f.graph, f.err
}

func TestRegistry_RoutesToRegisteredScheme(t *testing.T) {
	modGraph := graph.NewMemoryStore()
	gomodGraph := graph.NewMemoryStore()

	reg := NewRegistry()
	reg.Register("mod", &fakeResolver{graph: modGraph})
	reg.Register("gomod", &fakeResolver{graph: gomodGraph})

	cases := []struct {
		scheme string
		want   graph.Graph
	}{
		{"mod", modGraph},
		{"gomod", gomodGraph},
	}
	for _, tc := range cases {
		t.Run(tc.scheme, func(t *testing.T) {
			got, err := reg.Resolve(context.Background(), tc.scheme, "irrelevant-locator")
			require.NoError(t, err)
			assert.Same(t, tc.want, got, "must route to the resolver registered for %q, not any other", tc.scheme)
		})
	}
}

func TestRegistry_UnknownSchemeReturnsSentinel(t *testing.T) {
	reg := NewRegistry()
	reg.Register("mod", &fakeResolver{graph: graph.NewMemoryStore()})

	_, err := reg.Resolve(context.Background(), "npm", "react")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSchemeUnknown), "must be ErrSchemeUnknown even though wrapped with the scheme name")
	assert.Contains(t, err.Error(), "npm", "the error should name which scheme was missing")
}

func TestRegistry_PropagatesResolverError(t *testing.T) {
	reg := NewRegistry()
	reg.Register("gomod", &fakeResolver{err: ErrNotResolvable})

	_, err := reg.Resolve(context.Background(), "gomod", "example.com/not/a/real/module")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotResolvable))
}

// assertResolveIsCached calls resolve twice and asserts the second call
// returns the identical graph.Graph instance — the shared assertion behind
// every Resolver implementation's "CachesRepeatedResolution" test
// (GoModResolver, LocalPathResolver, ...).
func assertResolveIsCached(t *testing.T, resolve func() (graph.Graph, error)) {
	t.Helper()
	first, err := resolve()
	require.NoError(t, err)
	second, err := resolve()
	require.NoError(t, err)
	require.Same(t, first, second, "a cached Resolve must return the same graph instance, not rebuild")
}

func TestRegistry_LaterRegistrationReplacesEarlier(t *testing.T) {
	first := graph.NewMemoryStore()
	second := graph.NewMemoryStore()

	reg := NewRegistry()
	reg.Register("mod", &fakeResolver{graph: first})
	reg.Register("mod", &fakeResolver{graph: second})

	got, err := reg.Resolve(context.Background(), "mod", "x")
	require.NoError(t, err)
	assert.Same(t, second, got, "the second Register for the same scheme must win")
}
