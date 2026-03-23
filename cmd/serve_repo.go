package cmd

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// sanitizeSessionID ensures a session ID is safe for use as a filesystem path.
// Rejects path separators, "..", and empty strings. Returns error if unsafe.
func sanitizeSessionID(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("empty session ID")
	}
	if strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("unsafe session ID: %q", sessionID)
	}
	// Replace any remaining non-alphanumeric chars (except dash/underscore) for safety.
	return sessionID, nil
}

// createWorktree creates a git worktree for session isolation.
// The worktree shares the object store with cloneDir (instant, no network).
// cloneDir should be a "base" subdirectory so that sessions/ is a sibling
// under the same parent (cleaned up together on exit).
// Returns the worktree directory path.
func createWorktree(cloneDir, sessionID string) (string, error) {
	safe, err := sanitizeSessionID(sessionID)
	if err != nil {
		return "", err
	}

	// Sessions dir is a sibling of cloneDir under the same parent temp dir.
	// e.g., cloneDir=/tmp/mache-repo-xxx/base → sessions=/tmp/mache-repo-xxx/sessions
	sessionsDir := filepath.Join(filepath.Dir(cloneDir), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return "", fmt.Errorf("create sessions dir: %w", err)
	}

	wtDir := filepath.Join(sessionsDir, safe)
	cmd := exec.Command("git", "worktree", "add", wtDir, "HEAD")
	cmd.Dir = cloneDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %s: %w", string(out), err)
	}
	return wtDir, nil
}

// removeWorktree removes a git worktree and its directory.
// Falls back to rm -rf if git worktree remove fails.
func removeWorktree(cloneDir, worktreeDir string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreeDir)
	cmd.Dir = cloneDir
	if err := cmd.Run(); err != nil {
		log.Printf("git worktree remove failed, falling back to rm: %v", err)
		return os.RemoveAll(worktreeDir)
	}
	return nil
}

// repoWorktreePath returns the expected worktree path for a session.
// Does not create the worktree — that happens lazily on first tool call.
func (r *graphRegistry) repoWorktreePath(sessionID string) string {
	return filepath.Join(filepath.Dir(r.repoCloneDir), "sessions", sessionID)
}

// ensureRepoWorktree creates a worktree for the session if one doesn't exist.
// Thread-safe: uses LoadOrStore to serialize per-session creation.
func (r *graphRegistry) ensureRepoWorktree(sessionID string) (string, error) {
	// Fast path: already created.
	if wtDir, ok := r.worktrees.Load(sessionID); ok {
		return wtDir.(string), nil
	}

	// Serialize creation per session using a once-per-session mutex.
	oncei, _ := r.worktreeOnces.LoadOrStore(sessionID, &sync.Once{})
	once := oncei.(*sync.Once)

	var wtDir string
	var createErr error
	once.Do(func() {
		wtDir, createErr = createWorktree(r.repoCloneDir, sessionID)
		if createErr == nil {
			r.worktrees.Store(sessionID, wtDir)
		}
	})

	if createErr != nil {
		return "", createErr
	}
	// If another goroutine ran the Once, load the result.
	if wtDir == "" {
		if v, ok := r.worktrees.Load(sessionID); ok {
			return v.(string), nil
		}
		return "", fmt.Errorf("worktree creation failed for session %s", sessionID)
	}
	return wtDir, nil
}

// cleanupRepoSession removes a session's worktree, evicts cached graphs,
// and cleans up map entries.
func (r *graphRegistry) cleanupRepoSession(sessionID string) {
	wtDir, ok := r.worktrees.Load(sessionID)
	if !ok {
		return
	}
	wtPath := wtDir.(string)

	// Evict cached graph(s) for this worktree path.
	r.graphs.Range(func(key, value any) bool {
		keyStr := key.(string)
		if keyStr == wtPath || strings.HasPrefix(keyStr, wtPath+"@") {
			if lg, ok := value.(*lazyGraph); ok && lg.cleanup != nil {
				lg.cleanup()
			}
			r.graphs.Delete(key)
		}
		return true
	})

	if err := removeWorktree(r.repoCloneDir, wtPath); err != nil {
		log.Printf("cleanup worktree for session %s: %v", sessionID, err)
	}
	r.worktrees.Delete(sessionID)
	r.worktreeOnces.Delete(sessionID)
}
