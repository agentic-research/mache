package cmd

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// createWorktree creates a git worktree for session isolation.
// The worktree shares the object store with cloneDir (instant, no network).
// Returns the worktree directory path.
func createWorktree(cloneDir, sessionID string) (string, error) {
	sessionsDir := filepath.Join(filepath.Dir(cloneDir), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return "", fmt.Errorf("create sessions dir: %w", err)
	}

	wtDir := filepath.Join(sessionsDir, sessionID)
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

// cleanupRepoSession removes a session's worktree and cleans up the map entry.
func (r *graphRegistry) cleanupRepoSession(sessionID string) {
	wtDir, ok := r.worktrees.Load(sessionID)
	if !ok {
		return
	}
	if err := removeWorktree(r.repoCloneDir, wtDir.(string)); err != nil {
		log.Printf("cleanup worktree for session %s: %v", sessionID, err)
	}
	r.worktrees.Delete(sessionID)
}
