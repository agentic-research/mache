package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
	"golang.org/x/sync/singleflight"
)

// GoModResolver resolves a Go import path to the graph of its defining
// module, for the `gomod:` scheme ADR-0016 names but never implements.
//
// It deliberately does NOT hand-roll go.mod parsing, replace-directive
// handling, or semver resolution — `go list` is Go's own first-party
// module-resolution tool and already gets those right. This is the
// resolver-body pattern the companion ADR (docs/adr/0025) documents: shell
// out to the ecosystem's own canonical tool, don't reimplement its
// resolution semantics.
//
// Once `go list` names the resolved package's directory, that directory is
// indexed with graph.Build (leyline parse, this session's library facade —
// see docs/CHANGELOG under mache-3edd21) and opened with graph.Open, so
// LookupDef/QueryRefs/GetCallers work on the result exactly as they do on
// any other mache-produced .db.
type GoModResolver struct {
	// WorkDir is the directory `go list` runs from, which determines which
	// module's go.mod (and therefore which module graph) resolution
	// happens against. Required.
	WorkDir string

	cache sync.Map // import path -> *cacheEntry
	sf    singleflight.Group
}

type cacheEntry struct {
	g   graph.Graph
	err error
}

// goListPackage is the subset of `go list -json <pattern>`'s output this
// resolver needs. The full schema has ~40 fields; deliberately not
// modelling the rest — a resolver that doesn't need a field shouldn't
// silently start depending on it because a struct happened to have it.
type goListPackage struct {
	Dir   string
	Error *struct {
		Err string
	}
}

// Resolve implements Resolver for the gomod scheme. locator is a Go import
// path, e.g. "github.com/agentic-research/mache/graph".
func (r *GoModResolver) Resolve(ctx context.Context, locator string) (graph.Graph, error) {
	if r.WorkDir == "" {
		return nil, fmt.Errorf("gomod resolver: WorkDir is required")
	}

	if v, ok := r.cache.Load(locator); ok {
		e := v.(*cacheEntry)
		return e.g, e.err
	}

	// singleflight coalesces concurrent Resolve calls for the same import
	// path into one `go list` + graph.Build — both are real subprocess/IO
	// cost, and two callers racing to resolve the same locator should not
	// pay it twice.
	v, err, _ := r.sf.Do(locator, func() (any, error) {
		g, resolveErr := r.resolveUncached(ctx, locator)
		r.cache.Store(locator, &cacheEntry{g: g, err: resolveErr})
		return g, resolveErr
	})
	if err != nil {
		return nil, err
	}
	return v.(graph.Graph), nil
}

func (r *GoModResolver) resolveUncached(ctx context.Context, locator string) (graph.Graph, error) {
	dir, err := goListDir(ctx, r.WorkDir, locator)
	if err != nil {
		return nil, err
	}
	return buildAndOpen("mache-gomod-resolve-*", dir)
}

// goListDir shells out to `go list -json <locator>` and returns the
// resolved package's directory. Returns ErrNotResolvable — not a bare
// exec/parse error — for every case where the failure means "this import
// path does not resolve", so callers can distinguish "not resolvable" from
// "go list itself is broken" (missing toolchain, no network for an
// uncached module, etc.) by checking errors.Is separately if they need to.
func goListDir(ctx context.Context, workDir, locator string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-json", locator)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// `go list` can exit non-zero yet still emit a valid JSON object
	// describing the failure (pkg.Error) — parse before treating a
	// nonzero exit as fatal, so an unresolvable import path (the common
	// case) reports ErrNotResolvable instead of a raw exit-status error.
	var pkg goListPackage
	if jsonErr := json.Unmarshal(stdout.Bytes(), &pkg); jsonErr != nil {
		if runErr != nil {
			return "", fmt.Errorf("%w: go list %s: %v: %s", ErrNotResolvable, locator, runErr, stderr.String())
		}
		return "", fmt.Errorf("gomod resolver: parse go list output for %s: %w", locator, jsonErr)
	}
	if pkg.Error != nil {
		return "", fmt.Errorf("%w: %s", ErrNotResolvable, pkg.Error.Err)
	}
	if pkg.Dir == "" {
		return "", fmt.Errorf("%w: go list reported no directory for %s", ErrNotResolvable, locator)
	}
	return pkg.Dir, nil
}
