package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// repoClone tracks a shared base clone for a repo URL in hosted mode.
// Multiple sessions can share the same base clone; each gets its own
// worktree for isolation. When all sessions disconnect, a cleanup timer
// removes the clone after an idle TTL.
type repoClone struct {
	baseDir      string
	mu           sync.Mutex
	refCount     int
	cleanupTimer *time.Timer
}

const repoIdleTTL = 10 * time.Minute

// repoContextKey is a context key for the ?repo= URL query parameter.
type repoContextKey struct{}

// schemaContextKey is a context key for the ?schema= URL query parameter.
type schemaContextKey struct{}

// hostedContextFromRequest extracts ?repo= and ?schema= from the HTTP request
// URL and stashes them in context. This is the server.HTTPContextFunc for mcp-go.
//
// Query parameters:
//   - repo:   Git URL to clone and serve (e.g., https://github.com/org/repo)
//   - schema: Schema preset to use (e.g., "go", "python", "rust"). Skips
//     auto-detection/FCA inference. Typically set by an upstream middleware
//     that queries the GitHub languages API.
func hostedContextFromRequest(ctx context.Context, r *http.Request) context.Context {
	q := r.URL.Query()
	if repo := q.Get("repo"); repo != "" {
		ctx = context.WithValue(ctx, repoContextKey{}, repo)
	}
	if schema := q.Get("schema"); schema != "" {
		ctx = context.WithValue(ctx, schemaContextKey{}, schema)
	}
	return ctx
}

// repoFromContext extracts the repo URL from context, if present.
func repoFromContext(ctx context.Context) (string, bool) {
	repo, ok := ctx.Value(repoContextKey{}).(string)
	return repo, ok
}

// schemaFromContext extracts the schema preset from context, if present.
func schemaFromContext(ctx context.Context) (string, bool) {
	schema, ok := ctx.Value(schemaContextKey{}).(string)
	return schema, ok
}

// getOrCreateRepoClone returns the base clone dir for a repo URL.
// Clones on first access (git clone --depth=1), reuses on subsequent.
// Thread-safe via LoadOrStore.
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
	cmd.Dir = parentDir // ensure valid CWD for git subprocess
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
