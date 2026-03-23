# Hosted Index Pipeline (mache-d403d9)

**Date**: 2026-03-22
**Status**: Design
**Bead**: mache-d403d9

## Problem

`mache serve --repo <url>` works for CLI usage but hosted mache at `mache.rosary.bot` needs dynamic repo selection via HTTP query parameter. Multiple clients pointing at different repos should share base clones. Idle repos should be cleaned up.

## Design

### URL Format

```
https://mache.rosary.bot/mcp?repo=https://github.com/org/repo
```

Clients connect via: `claude mcp add --transport http mache "https://mache.rosary.bot/mcp?repo=https://github.com/org/repo"`

Browser visit without `?repo=` serves a landing page (rig repo concern, not mache code).

### Lifecycle

```
Request arrives: /mcp?repo=https://github.com/org/repo
  → Extract ?repo= from HTTP URL
  → graphRegistry.getOrCreateRepoClone(repoURL)
    → HIT: reuse existing base clone, increment refcount
    → MISS: git clone --depth=1 → /tmp/mache-hosted-<hash>/base/
  → Session gets worktree off base clone (PR #120 mechanism)
  → lazyGraph ingests on first tool call
  → Session disconnects: decrement refcount
  → refcount=0: schedule cleanup after idle TTL
  → TTL expires with no new sessions: os.RemoveAll
```

### Changes

#### `serve_registry.go` — repo clone cache

```go
type repoClone struct {
    baseDir    string
    mu         sync.Mutex
    refCount   int
    cleanupTimer *time.Timer
}
```

New field on `graphRegistry`:

- `repoClones sync.Map` — repo URL → `*repoClone`

New methods:

- `getOrCreateRepoClone(repoURL string) (string, error)` — clone if missing, increment refcount
- `releaseRepoClone(repoURL string)` — decrement refcount, schedule idle cleanup

#### `serve.go` — HTTP middleware for `?repo=`

Wrap the Streamable HTTP handler to extract `?repo=` from the request URL and stash the repo URL in context. The MCP server sees a normal request; the repo URL is available to `wrapHandler` via context.

```go
type repoContextKey struct{}

func repoMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if repo := r.URL.Query().Get("repo"); repo != "" {
            ctx := context.WithValue(r.Context(), repoContextKey{}, repo)
            r = r.WithContext(ctx)
        }
        next.ServeHTTP(w, r)
    })
}
```

#### `serve_registry.go` — `wrapHandler` reads repo from context

In `wrapHandler`, before resolving the session's graph:

1. Check context for repo URL
1. If present, call `getOrCreateRepoClone(repoURL)` to get/create base clone
1. Set `repoCloneDir` on the registry for this session's worktree creation
1. If not present, use existing resolution (CLI `--repo` or local path)

#### `serve_registry.go` — `unregisterSession` releases clone

When a session disconnects, call `releaseRepoClone` to decrement refcount and schedule idle cleanup.

### Idle Cleanup

- Each `repoClone` has a `refCount` and a `cleanupTimer`
- `getOrCreateRepoClone`: increments refcount, cancels any pending timer
- `releaseRepoClone`: decrements refcount, if zero starts a 10-minute timer
- Timer fires: remove clone dir + worktrees, delete from `repoClones` map
- New session for same repo before timer: cancel timer, increment refcount

### What Doesn't Change

- `--repo` CLI flag (single-repo mode)
- `--stdio` mode
- Non-repo HTTP mode (serve local dir)
- Per-session worktrees (PR #120)

### Error Handling

- Invalid/unreachable repo URL → MCP error on first tool call
- Clone timeout (e.g., massive repo) → MCP error with message
- Disk full → MCP error
- No `?repo=` and no CLI `--repo` → normal local path resolution

### Tests

- `TestExtractRepoFromURL` — parse `?repo=` from various URL formats
- `TestRepoCloneCache_Hit` — second session reuses existing clone
- `TestRepoCloneCache_Miss` — new URL triggers new clone
- `TestRepoCloneCache_RefCounting` — cleanup only after all sessions disconnect
- `TestRepoCloneCache_IdleCleanup` — clone removed after TTL with no sessions
- `TestNoRepoParam_FallsThrough` — requests without `?repo=` use normal path

## Scope

~150 LOC implementation in `serve.go` + `serve_registry.go`, ~100 LOC tests. Landing page is rig repo work, not in scope here.
