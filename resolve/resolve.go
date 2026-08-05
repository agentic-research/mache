// Package resolve provides the public cross-language ref resolver API for
// mache (ADR-0016).
//
// Types are defined in internal/resolve and re-exported here via type
// aliases so external consumers (e.g. a tool that wants to resolve a Go
// import path or a local relative path to a queryable graph.Graph) can use
// them without importing internal packages — the same pattern graph.Open /
// graph.Build already established for the graph package.
package resolve

import (
	ir "github.com/agentic-research/mache/internal/resolve"
)

// Resolver resolves one scheme's locators to a graph.Graph. Implementations
// own their own caching.
type Resolver = ir.Resolver

// Registry routes a (scheme, locator) pair to the Resolver registered for
// that scheme.
type Registry = ir.Registry

// GoModResolver resolves a Go import path to the graph of its defining
// module via `go list` + graph.Build + graph.Open. WorkDir (required) is the
// directory `go list` runs from.
type GoModResolver = ir.GoModResolver

// LocalPathResolver resolves `./X` / `../X` locators (or an
// already-anchored absolute path) to the graph of the directory they name,
// via graph.Build + graph.Open. Anchor (required for relative locators) is
// the directory relative locators are resolved against.
type LocalPathResolver = ir.LocalPathResolver

// NewRegistry returns an empty Registry ready for Register calls.
var NewRegistry = ir.NewRegistry

// ErrNotResolvable is returned by a Resolver when the locator is
// syntactically in its scheme but does not resolve to anything.
var ErrNotResolvable = ir.ErrNotResolvable

// ErrSchemeUnknown is returned by Registry.Resolve when no Resolver has been
// registered for the requested scheme.
var ErrSchemeUnknown = ir.ErrSchemeUnknown
