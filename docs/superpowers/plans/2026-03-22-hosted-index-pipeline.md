# Hosted Index Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable `mache serve` to accept `?repo=<url>` as an HTTP query parameter, dynamically cloning repos on first request with shared base clones and idle cleanup.

**Architecture:** HTTP middleware extracts `?repo=` from request URL via `mcp-go`'s `WithHTTPContextFunc`. A `repoCloneCache` on `graphRegistry` keys by repo URL, refcounts active sessions, and schedules cleanup after idle TTL. Per-session worktrees (PR #120) are reused for isolation.

**Tech Stack:** Go, mcp-go (StreamableHTTPServer, WithHTTPContextFunc), git CLI

______________________________________________________________________

### Task 1: repoClone Type + Cache Methods

**Files:**

- Create: `cmd/serve_hosted.go`

- Test: `cmd/serve_hosted_test.go`

- [ ] **Step 1: Write failing test for getOrCreateRepoClone**

```go
func TestRepoCloneCache_NewClone(t *testing.T) {
    cloneDir := initTestGitRepo(t) // from serve_repo_test.go

    r := newGraphRegistry("", nil)
    baseDir, err := r.getOrCreateRepoClone(cloneDir) // use local path as "URL" for test
    require.NoError(t, err)
    assert.DirExists(t, baseDir)

    // Should contain committed files
    _, err = os.Stat(filepath.Join(baseDir, "main.go"))
    assert.NoError(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `task test -- -run TestRepoCloneCache_NewClone -v`
Expected: FAIL with "getOrCreateRepoClone not defined"

- [ ] **Step 3: Write repoClone type and cache methods**

In `cmd/serve_hosted.go`:

```go
package cmd

import (
    "fmt"
    "log"
    "os"
    "os/exec"
    "path/filepath"
    "sync"
    "time"
)

type repoClone struct {
    baseDir      string
    mu           sync.Mutex
    refCount     int
    cleanupTimer *time.Timer
}

const repoIdleTTL = 10 * time.Minute

// getOrCreateRepoClone returns the base clone dir for a repo URL.
// Clones on first access, reuses on subsequent. Thread-safe.
func (r *graphRegistry) getOrCreateRepoClone(repoURL string) (string, error) {
    // Fast path: already cloned
    if v, ok := r.repoClones.Load(repoURL); ok {
        rc := v.(*repoClone)
        rc.mu.Lock()
        rc.refCount++
        if rc.cleanupTimer != nil {
            rc.cleanupTimer.Stop()
            rc.cleanupTimer = nil
        }
        rc.mu.Unlock()
        return rc.baseDir, nil
    }

    // Slow path: clone
    parentDir, err := os.MkdirTemp("", "mache-hosted-*")
    if err != nil {
        return "", fmt.Errorf("create temp dir: %w", err)
    }
    baseDir := filepath.Join(parentDir, "base")

    log.Printf("cloning %s for hosted mode...", repoURL)
    cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", repoURL, baseDir)
    cmd.Stdout = os.Stderr
    cmd.Stderr = os.Stderr
    if err := cmd.Run(); err != nil {
        _ = os.RemoveAll(parentDir)
        return "", fmt.Errorf("git clone %s: %w", repoURL, err)
    }

    rc := &repoClone{baseDir: baseDir, refCount: 1}
    if existing, loaded := r.repoClones.LoadOrStore(repoURL, rc); loaded {
        // Another goroutine cloned simultaneously — use theirs, discard ours
        _ = os.RemoveAll(parentDir)
        existingRC := existing.(*repoClone)
        existingRC.mu.Lock()
        existingRC.refCount++
        existingRC.mu.Unlock()
        return existingRC.baseDir, nil
    }

    log.Printf("cloned %s → %s", repoURL, baseDir)
    return baseDir, nil
}

// releaseRepoClone decrements the refcount for a repo.
// When refcount hits 0, schedules cleanup after idle TTL.
func (r *graphRegistry) releaseRepoClone(repoURL string) {
    v, ok := r.repoClones.Load(repoURL)
    if !ok {
        return
    }
    rc := v.(*repoClone)
    rc.mu.Lock()
    defer rc.mu.Unlock()

    rc.refCount--
    if rc.refCount > 0 {
        return
    }

    rc.cleanupTimer = time.AfterFunc(repoIdleTTL, func() {
        log.Printf("idle cleanup: removing clone for %s", repoURL)
        r.repoClones.Delete(repoURL)
        _ = os.RemoveAll(filepath.Dir(rc.baseDir)) // remove parent (base + sessions)
    })
}
```

- [ ] **Step 4: Add repoClones field to graphRegistry**

In `cmd/serve_registry.go`, add to `graphRegistry` struct:

```go
repoClones sync.Map // repo URL → *repoClone (hosted mode cache)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `task test -- -run TestRepoCloneCache -v`
Expected: PASS

- [ ] **Step 6: Write additional cache tests**

```go
func TestRepoCloneCache_Hit(t *testing.T) {
    cloneDir := initTestGitRepo(t)
    r := newGraphRegistry("", nil)

    dir1, err := r.getOrCreateRepoClone(cloneDir)
    require.NoError(t, err)
    dir2, err := r.getOrCreateRepoClone(cloneDir)
    require.NoError(t, err)

    assert.Equal(t, dir1, dir2, "same URL should return same dir")
}

func TestRepoCloneCache_DifferentURLs(t *testing.T) {
    clone1 := initTestGitRepo(t)
    clone2 := initTestGitRepo(t)
    r := newGraphRegistry("", nil)

    dir1, _ := r.getOrCreateRepoClone(clone1)
    dir2, _ := r.getOrCreateRepoClone(clone2)

    assert.NotEqual(t, dir1, dir2, "different URLs should get different dirs")
}

func TestRepoCloneCache_RefCounting(t *testing.T) {
    cloneDir := initTestGitRepo(t)
    r := newGraphRegistry("", nil)

    dir, _ := r.getOrCreateRepoClone(cloneDir)
    _, _ = r.getOrCreateRepoClone(cloneDir) // refcount = 2

    r.releaseRepoClone(cloneDir) // refcount = 1
    assert.DirExists(t, dir, "should still exist with refcount > 0")

    r.releaseRepoClone(cloneDir) // refcount = 0, timer starts
    // Dir still exists (timer hasn't fired)
    assert.DirExists(t, dir)
}
```

- [ ] **Step 7: Run all tests, verify pass**

Run: `task test -- -run TestRepoCloneCache -v`

- [ ] **Step 8: Commit**

```bash
git add cmd/serve_hosted.go cmd/serve_hosted_test.go cmd/serve_registry.go
git commit -m "feat: repoCloneCache with refcounting and idle cleanup"
```

______________________________________________________________________

### Task 2: HTTP Middleware — Extract `?repo=` via WithHTTPContextFunc

**Files:**

- Modify: `cmd/serve.go`

- Test: `cmd/serve_hosted_test.go`

- [ ] **Step 1: Write failing test for repo context extraction**

```go
type repoContextKey struct{}

func TestExtractRepoFromContext(t *testing.T) {
    req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo", nil)
    ctx := repoContextFromRequest(req.Context(), req)
    repo, ok := ctx.Value(repoContextKey{}).(string)
    require.True(t, ok)
    assert.Equal(t, "https://github.com/org/repo", repo)
}

func TestExtractRepoFromContext_NoParam(t *testing.T) {
    req := httptest.NewRequest("POST", "/mcp", nil)
    ctx := repoContextFromRequest(req.Context(), req)
    _, ok := ctx.Value(repoContextKey{}).(string)
    assert.False(t, ok, "no repo param means no context value")
}
```

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Write the context function**

In `cmd/serve_hosted.go`:

```go
type repoContextKey struct{}

// repoContextFromRequest extracts ?repo= from the URL and stashes it in context.
// Used as mcp-go's WithHTTPContextFunc.
func repoContextFromRequest(ctx context.Context, r *http.Request) context.Context {
    if repo := r.URL.Query().Get("repo"); repo != "" {
        return context.WithValue(ctx, repoContextKey{}, repo)
    }
    return ctx
}

// repoFromContext extracts the repo URL from context, if present.
func repoFromContext(ctx context.Context) (string, bool) {
    repo, ok := ctx.Value(repoContextKey{}).(string)
    return repo, ok
}
```

- [ ] **Step 4: Run test to verify pass**

- [ ] **Step 5: Wire into runServe**

In `cmd/serve.go`, change `NewStreamableHTTPServer` call:

```go
httpServer := server.NewStreamableHTTPServer(s,
    server.WithHTTPContextFunc(repoContextFromRequest),
)
```

- [ ] **Step 6: Run full build + test**

Run: `task build && task test`

- [ ] **Step 7: Commit**

```bash
git add cmd/serve.go cmd/serve_hosted.go cmd/serve_hosted_test.go
git commit -m "feat: extract ?repo= from HTTP URL via WithHTTPContextFunc"
```

______________________________________________________________________

### Task 3: Wire wrapHandler to Use Repo Clone Cache

**Files:**

- Modify: `cmd/serve_registry.go`

- Test: `cmd/serve_hosted_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestWrapHandler_RepoFromContext(t *testing.T) {
    cloneDir := initTestGitRepo(t)
    r := newGraphRegistry("", nil)

    // Simulate a request with ?repo= in context
    handler := r.wrapHandler(makeListDirHandler)

    req := mcp.CallToolRequest{}
    req.Params.Arguments = map[string]any{"path": ""}

    ctx := context.WithValue(context.Background(), repoContextKey{}, cloneDir)
    // Need to attach a mock session too — check existing test patterns
}
```

- [ ] **Step 2: Modify resolveSession to check context for repo URL**

In `cmd/serve_registry.go`, in `resolveSession`, add BEFORE the repo worktree block:

```go
// Hosted mode: ?repo= URL from HTTP context
if repoURL, ok := repoFromContext(ctx); ok {
    baseDir, err := r.getOrCreateRepoClone(repoURL)
    if err != nil {
        log.Printf("clone %s for session %s: %v", repoURL, sid, err)
        // Return error graph or fallback
    } else {
        // Store repo URL on session for cleanup
        r.sessionRepos.Store(sid, repoURL)
        // Create worktree off this base clone
        wtDir, err := r.ensureRepoWorktree(sid)
        // ... same as existing worktree logic but with dynamic repoCloneDir
    }
}
```

The challenge: `ensureRepoWorktree` currently uses `r.repoCloneDir` (a single value). For hosted mode, each session can have a DIFFERENT base clone. Solution: pass the base clone dir through, or look it up from `sessionRepos`.

- [ ] **Step 3: Add sessionRepos field**

In `serve_registry.go`:

```go
sessionRepos sync.Map // sessionID → repo URL (for hosted mode cleanup)
```

- [ ] **Step 4: Modify ensureRepoWorktree to accept baseDir parameter**

Change `ensureRepoWorktree` to take a `baseDir` parameter instead of using `r.repoCloneDir`:

```go
func (r *graphRegistry) ensureRepoWorktreeFrom(sessionID, baseDir string) (string, error)
```

Update existing callers in `resolveSession` to pass `r.repoCloneDir` for CLI mode, or the dynamic baseDir for hosted mode.

- [ ] **Step 5: Wire unregisterSession to release clone**

In `serve_registry.go`, in `unregisterSession`:

```go
if repoURL, ok := r.sessionRepos.LoadAndDelete(sid); ok {
    r.releaseRepoClone(repoURL.(string))
}
```

- [ ] **Step 6: Run full test suite**

Run: `task test`

- [ ] **Step 7: Commit**

```bash
git add cmd/serve_registry.go cmd/serve_hosted.go cmd/serve_hosted_test.go cmd/serve_repo.go
git commit -m "feat: wrapHandler resolves ?repo= from context, creates per-session worktrees"
```

______________________________________________________________________

### Task 4: Integration Test — End-to-End Hosted Flow

**Files:**

- Test: `cmd/serve_hosted_test.go`

- [ ] **Step 1: Write integration test**

```go
func TestHosted_EndToEnd(t *testing.T) {
    // Create a local git repo as the "remote"
    repoDir := initTestGitRepo(t)

    r := newGraphRegistry("", nil)
    defer r.Close()

    // Simulate: session 1 connects with ?repo=repoDir
    baseDir1, err := r.getOrCreateRepoClone(repoDir)
    require.NoError(t, err)

    // Simulate: session 2 connects with same repo
    baseDir2, err := r.getOrCreateRepoClone(repoDir)
    require.NoError(t, err)
    assert.Equal(t, baseDir1, baseDir2, "same repo shares base clone")

    // Both sessions disconnect
    r.releaseRepoClone(repoDir)
    r.releaseRepoClone(repoDir)

    // Base dir still exists (cleanup timer hasn't fired)
    assert.DirExists(t, baseDir1)
}

func TestHosted_NoRepoParam_NormalPath(t *testing.T) {
    // Without ?repo=, normal resolution path is used
    r := newGraphRegistry(".", nil)
    lg := r.getOrCreateGraph(".")
    assert.NotNil(t, lg)
}
```

- [ ] **Step 2: Run test**

Run: `task test -- -run TestHosted -v`

- [ ] **Step 3: Run full suite**

Run: `task test`

- [ ] **Step 4: Commit**

```bash
git add cmd/serve_hosted_test.go
git commit -m "test: end-to-end hosted mode integration tests"
```

______________________________________________________________________

### Task 5: Final Wiring + Push

**Files:**

- Modify: `cmd/serve.go` (landing page redirect)

- [ ] **Step 1: Add landing page redirect for browser visits**

In `cmd/serve.go`, when setting up the HTTP mux, add a handler for GET `/` without MCP headers that returns a redirect or simple HTML:

```go
mux := http.NewServeMux()
mux.Handle("/mcp", httpServer)
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    // Browser visit without MCP — redirect to docs or show usage
    w.Header().Set("Content-Type", "text/plain")
    fmt.Fprintln(w, "mache MCP server")
    fmt.Fprintln(w, "Connect: claude mcp add --transport http mache \""+r.Host+"/mcp?repo=<your-repo-url>\"")
})
```

Note: The actual landing page HTML lives in rig's `site/mache/`. This is just a plain-text fallback.

- [ ] **Step 2: Run full test suite**

Run: `task build && task test`

- [ ] **Step 3: Final commit and push**

```bash
git add .
git commit -m "feat: hosted index pipeline — ?repo= query param for dynamic repo serving"
git push -u origin feat/hosted-index-pipeline
```

- [ ] **Step 4: Create PR**

```bash
gh pr create --title "feat: hosted index pipeline — ?repo= query param" --body "..."
```

- [ ] **Step 5: Close bead**

Close mache-d403d9 after merge.
