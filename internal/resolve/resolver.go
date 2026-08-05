// Package resolve implements ADR-0016's cross-language reference resolver:
// an open scheme registry where each scheme (mod, gomod, npm, git, ...) has
// an independent Resolver that turns a locator into the graph it names.
//
// This file is the contract every scheme's resolver builds on
// (mache-bd97d9). Pure Go, no CGO or tree-sitter dependency at this layer —
// individual resolvers (in their own files) may depend on more; this file
// depends on internal/graph only.
package resolve

import (
	"context"
	"errors"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
)

// ErrNotResolvable is returned by a Resolver when the locator is
// syntactically in its scheme but does not resolve to anything — e.g. an
// import path with no matching module, or a relative path that escapes its
// anchor. Distinct from ErrSchemeUnknown, which the Registry returns when no
// Resolver is registered for the scheme at all.
var ErrNotResolvable = errors.New("resolve: locator not resolvable")

// ErrSchemeUnknown is returned by Registry.Resolve when no Resolver has been
// registered for the requested scheme.
var ErrSchemeUnknown = errors.New("resolve: unknown scheme")

// Resolver resolves one scheme's locators to a graph. Implementations own
// their own caching — the Registry deliberately has none, so two lookups of
// the same locator are only as cheap as the Resolver makes them.
type Resolver interface {
	Resolve(ctx context.Context, locator string) (graph.Graph, error)
}

// Registry routes a (scheme, locator) pair to the Resolver registered for
// that scheme. It is an explicit object, not package-level state — callers
// construct one, register schemes, and pass it to whatever needs
// resolution (an MCP handler, a resolver that itself composes other
// resolvers), so tests never fight init-order or global mutation.
type Registry struct {
	resolvers sync.Map // scheme string -> Resolver
}

// NewRegistry returns an empty Registry ready for Register calls.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register associates scheme with res. A later call for the same scheme
// replaces the earlier registration — the last registration for a scheme
// wins, matching sync.Map's Store semantics; callers that need
// register-once should check first with their own bookkeeping.
func (r *Registry) Register(scheme string, res Resolver) {
	r.resolvers.Store(scheme, res)
}

// Resolve routes to the Resolver registered for scheme and returns its
// result. Returns ErrSchemeUnknown, wrapped so errors.Is still matches, when
// no Resolver is registered for scheme.
func (r *Registry) Resolve(ctx context.Context, scheme, locator string) (graph.Graph, error) {
	v, ok := r.resolvers.Load(scheme)
	if !ok {
		return nil, errUnknownScheme(scheme)
	}
	return v.(Resolver).Resolve(ctx, locator)
}

// errUnknownScheme wraps ErrSchemeUnknown with the scheme that was missing,
// so callers see which scheme failed without losing errors.Is(err,
// ErrSchemeUnknown) matchability.
func errUnknownScheme(scheme string) error {
	return &schemeError{scheme: scheme, err: ErrSchemeUnknown}
}

type schemeError struct {
	scheme string
	err    error
}

func (e *schemeError) Error() string { return e.err.Error() + ": " + e.scheme }
func (e *schemeError) Unwrap() error { return e.err }
