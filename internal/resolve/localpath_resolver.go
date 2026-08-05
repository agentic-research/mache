package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
	"golang.org/x/sync/singleflight"
)

// LocalPathResolver resolves `./X` / `../X` locators (and, for callers that
// have already done the joining themselves, absolute paths) to the graph of
// the directory they name — the `mod` scheme ADR-0016 names for
// local-relative references such as a Terraform `module { source = "./x" }`.
//
// Like GoModResolver, it deliberately does not hand-roll ingestion: once a
// locator resolves to a directory, graph.Build (leyline parse) + graph.Open
// index and open it exactly as any other mache-produced .db, so the result
// supports LookupDef/QueryRefs/GetCallers immediately.
type LocalPathResolver struct {
	// Anchor is the absolute directory a relative locator (./X, ../X) is
	// resolved against. Required for relative locators; ignored for
	// locators that are already absolute (a caller that has already
	// anchored the path itself, e.g. resolve_ref's base_path handling).
	Anchor string

	cache sync.Map // absolute path -> *cacheEntry
	sf    singleflight.Group
}

// Resolve implements Resolver for the mod scheme. A relative locator is
// joined against Anchor and rejected with ErrNotResolvable if it escapes
// it (e.g. "../../../etc"); an absolute locator is used as-is — the caller
// (not this resolver) is responsible for having validated it.
func (r *LocalPathResolver) Resolve(ctx context.Context, locator string) (graph.Graph, error) {
	target, err := r.anchoredPath(locator)
	if err != nil {
		return nil, err
	}

	if v, ok := r.cache.Load(target); ok {
		e := v.(*localCacheEntry)
		return e.g, e.err
	}

	v, err, _ := r.sf.Do(target, func() (any, error) {
		g, resolveErr := r.resolveUncached(target)
		r.cache.Store(target, &localCacheEntry{g: g, err: resolveErr})
		return g, resolveErr
	})
	if err != nil {
		return nil, err
	}
	return v.(graph.Graph), nil
}

// anchoredPath validates locator and returns the absolute directory it
// names. IsLocalRelativeLocator lives here (moved from
// cmd/serve_resolve_ref.go, mache-be0b9f) as the one place that decides
// what counts as a local-relative locator for the mod scheme.
func (r *LocalPathResolver) anchoredPath(locator string) (string, error) {
	if filepath.IsAbs(locator) {
		return filepath.Clean(locator), nil
	}
	if !IsLocalRelativeLocator(locator) {
		return "", fmt.Errorf("%w: %q is not a local relative path", ErrNotResolvable, locator)
	}
	if r.Anchor == "" {
		return "", fmt.Errorf("localpath resolver: Anchor is required to resolve relative locator %q", locator)
	}
	anchor, err := filepath.Abs(r.Anchor)
	if err != nil {
		return "", fmt.Errorf("localpath resolver: resolve anchor %q: %w", r.Anchor, err)
	}
	target, err := filepath.Abs(filepath.Join(anchor, locator))
	if err != nil {
		return "", fmt.Errorf("localpath resolver: resolve %q: %w", locator, err)
	}
	if target != anchor && !strings.HasPrefix(target, anchor+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes anchor %q", ErrNotResolvable, locator, r.Anchor)
	}
	return target, nil
}

// IsLocalRelativeLocator reports whether the locator should be resolved
// against the local filesystem. Conservative: only ./ and ../ prefixes.
// Bare paths like "foo/bar" are ambiguous (could be a registry slug); those
// are treated as remote until a registry resolver is added.
//
// Exported (moved from cmd/serve_resolve_ref.go, mache-be0b9f) so both this
// resolver and the resolve_ref MCP handler's own pre-flight local-path
// listing share one definition of "local".
func IsLocalRelativeLocator(locator string) bool {
	return strings.HasPrefix(locator, "./") || strings.HasPrefix(locator, "../")
}

func (r *LocalPathResolver) resolveUncached(dir string) (graph.Graph, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: stat %s: %v", ErrNotResolvable, dir, err)
	}
	if !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	return buildAndOpen("mache-localpath-resolve-*", dir)
}

type localCacheEntry struct {
	g   graph.Graph
	err error
}
