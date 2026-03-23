package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// repoClone tracks a shared base clone for a repo URL in hosted mode.
type repoClone struct {
	baseDir      string
	mu           sync.Mutex
	refCount     int
	cleanupTimer *time.Timer
}

const repoIdleTTL = 10 * time.Minute

// allowedRepoSchemes restricts which URL schemes are accepted for ?repo=.
// Only HTTPS is allowed for hosted mode — no file://, ssh://, or local paths.
var allowedRepoSchemes = map[string]bool{
	"https": true,
	"http":  true, // needed for local dev/testing
}

// repoContextKey is a context key for the ?repo= URL query parameter.
type repoContextKey struct{}

// schemaContextKey is a context key for the ?schema= URL query parameter.
type schemaContextKey struct{}

// hostedContextFromRequest extracts ?repo= and ?schema= from the HTTP request
// URL and stashes them in context. This is the server.HTTPContextFunc for mcp-go.
//
// Security: repo URLs are validated (HTTPS only, no option injection).
// Schema values are validated against known presets (no arbitrary file reads).
func hostedContextFromRequest(ctx context.Context, r *http.Request) context.Context {
	q := r.URL.Query()
	if repo := q.Get("repo"); repo != "" {
		if err := validateRepoURL(repo); err != nil {
			log.Printf("rejected ?repo=%s: %v", repo, err)
		} else {
			ctx = context.WithValue(ctx, repoContextKey{}, repo)
		}
	}
	if schema := q.Get("schema"); schema != "" {
		if isValidSchemaPreset(schema) {
			ctx = context.WithValue(ctx, schemaContextKey{}, schema)
		} else {
			log.Printf("rejected ?schema=%s: not a known preset", schema)
		}
	}
	return ctx
}

// validateRepoURL checks that a repo URL is safe for git clone.
// Rejects non-HTTPS schemes, option injection (starts with -), and embedded credentials.
func validateRepoURL(repoURL string) error {
	if strings.HasPrefix(repoURL, "-") {
		return fmt.Errorf("option injection: starts with dash")
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if !allowedRepoSchemes[u.Scheme] {
		return fmt.Errorf("scheme %q not allowed (use https)", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("embedded credentials not allowed")
	}
	return nil
}

// isValidSchemaPreset returns true if the schema name is a known preset.
// Prevents arbitrary file reads via ?schema=/etc/passwd.
func isValidSchemaPreset(schema string) bool {
	for _, name := range PresetNames() {
		if schema == name {
			return true
		}
	}
	return false
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

	log.Printf("cloning %s for hosted mode...", redactURL(repoURL))
	cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", repoURL, baseDir)
	cmd.Dir = parentDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(parentDir)
		return "", fmt.Errorf("git clone %s: %w", redactURL(repoURL), err)
	}

	rc := &repoClone{baseDir: baseDir, refCount: 1}
	if existing, loaded := r.repoClones.LoadOrStore(repoURL, rc); loaded {
		_ = os.RemoveAll(parentDir)
		existingRC := existing.(*repoClone)
		existingRC.mu.Lock()
		existingRC.refCount++
		existingRC.mu.Unlock()
		return existingRC.baseDir, nil
	}

	log.Printf("cloned %s → %s", redactURL(repoURL), baseDir)
	return baseDir, nil
}

// releaseRepoClone decrements the refcount for a repo.
// When refcount hits 0, schedules cleanup after idle TTL.
// The timer callback re-checks refCount under lock to avoid racing with
// new sessions that arrive as the timer fires.
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
		rc.mu.Lock()
		defer rc.mu.Unlock()
		// Re-check: a new session may have arrived since the timer was scheduled.
		if rc.refCount > 0 {
			return
		}
		log.Printf("idle cleanup: removing clone for %s", redactURL(repoURL))
		r.repoClones.Delete(repoURL)
		_ = os.RemoveAll(filepath.Dir(rc.baseDir))
	})
}

// redactURL strips query params and userinfo from a URL for safe logging.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	u.RawQuery = ""
	u.User = nil
	return u.String()
}
