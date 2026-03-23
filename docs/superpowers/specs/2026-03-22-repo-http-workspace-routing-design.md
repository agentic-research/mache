# HTTP Session → Workspace Routing for `--repo` Mode

**Date**: 2026-03-22
**Status**: Design
**Beads**: mache-2ef026 (P0), mache-ae1149 (P1)

## Problem

`mache serve --repo <url>` works for stdio (single session, single clone). In HTTP mode, multiple sessions can connect concurrently. Currently all sessions share one clone — no isolation. If one session's agent modifies files, other sessions see the mutations.

## Design

### Lifecycle

```
Startup:   git clone <url> → /tmp/mache-repo-<hash>/base/     (once)
Connect:   (nothing — lazy)
1st tool:  git worktree add .../sessions/<sessionID>/ HEAD     (instant, shared object store)
           → lazyGraph created with worktree as basePath
Disconnect: git worktree remove + rm dir
Exit:      rm -rf /tmp/mache-repo-<hash>/                     (base + all worktrees)
```

### Why Worktrees

`git worktree add` reuses the base clone's object store. No network, no disk copy of objects. ~instant for any repo size. Same pattern rosary uses for agent dispatch.

### Changes

**`graphRegistry` — two new fields:**

- `repoCloneDir string` — path to base clone (empty when not in repo mode)
- `worktrees sync.Map` — sessionID → worktree path (for cleanup)

**`resolveSession` — new branch:** When `repoCloneDir != ""` and session has no graph yet, call `createWorktree` instead of using shared basePath.

**`unregisterSession` — extend:** If session has a worktree entry, call `removeWorktree` and delete the map entry.

**`runServe` — extend:** When `--repo` is set and NOT `--stdio`, clone once and store `repoCloneDir` on registry. Don't set global `servePath`.

### New Functions

```go
// createWorktree creates a git worktree for session isolation.
// Returns the worktree directory path.
func createWorktree(cloneDir, sessionID string) (string, error)

// removeWorktree removes a git worktree and its directory.
func removeWorktree(cloneDir, worktreeDir string) error
```

Both shell out to `git worktree add/remove`. Worktree dirs: `<parentDir>/sessions/<sessionID>/`.

### What Doesn't Change

- `--stdio` mode: single clone, single session, no worktrees
- Non-repo mode: completely unchanged
- Existing tests: no behavior change

### Error Handling

- `git worktree add` fails → tool call returns MCP error (not a crash)
- `git worktree remove` fails on disconnect → log warning, rm -rf as fallback
- Process killed without cleanup → OS cleans temp dir eventually

### Tests

- `TestCreateWorktree` — creates worktree from a temp git repo, verifies dir exists
- `TestRemoveWorktree` — removes it, verifies dir gone
- `TestCreateWorktree_InvalidDir` — fails gracefully on non-git dir
- `TestRepoHTTP_SessionIsolation` — two sessions get different worktree paths
- `TestRepoHTTP_CleanupOnDisconnect` — worktree removed after unregister
- `TestRepoStdio_NoWorktree` — stdio mode doesn't create worktrees

## Implementation

~100-150 LOC in `cmd/serve.go` (or new `cmd/serve_repo.go`), ~100 LOC tests.
