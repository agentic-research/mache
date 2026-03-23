package cmd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// repoClone cache tests
// ---------------------------------------------------------------------------

func TestRepoCloneCache_NewClone(t *testing.T) {
	cloneDir := initTestGitRepo(t)  // from serve_repo_test.go
	repoURL := "file://" + cloneDir // git clone --depth=1 requires file:// for local repos

	r := newGraphRegistry("", nil)
	baseDir, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	assert.DirExists(t, baseDir)

	// Should contain committed files
	_, err = os.Stat(filepath.Join(baseDir, "main.go"))
	assert.NoError(t, err, "clone should contain committed files")
}

func TestRepoCloneCache_Hit(t *testing.T) {
	cloneDir := initTestGitRepo(t)
	repoURL := "file://" + cloneDir
	r := newGraphRegistry("", nil)

	dir1, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	dir2, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)

	assert.Equal(t, dir1, dir2, "same URL should return same dir")
}

func TestRepoCloneCache_DifferentURLs(t *testing.T) {
	clone1 := initTestGitRepo(t)
	clone2 := initTestGitRepo(t)
	r := newGraphRegistry("", nil)

	dir1, err := r.getOrCreateRepoClone("file://" + clone1)
	require.NoError(t, err)
	dir2, err := r.getOrCreateRepoClone("file://" + clone2)
	require.NoError(t, err)

	assert.NotEqual(t, dir1, dir2, "different URLs should get different dirs")
}

func TestRepoCloneCache_RefCounting(t *testing.T) {
	cloneDir := initTestGitRepo(t)
	repoURL := "file://" + cloneDir
	r := newGraphRegistry("", nil)

	dir, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	_, err = r.getOrCreateRepoClone(repoURL) // refcount = 2
	require.NoError(t, err)

	r.releaseRepoClone(repoURL) // refcount = 1
	assert.DirExists(t, dir, "should still exist with refcount > 0")

	r.releaseRepoClone(repoURL) // refcount = 0, timer starts
	// Dir still exists (timer hasn't fired yet)
	assert.DirExists(t, dir)
}

// ---------------------------------------------------------------------------
// repo context extraction tests
// ---------------------------------------------------------------------------

func TestExtractRepoFromContext(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo", nil)
	ctx := hostedContextFromRequest(req.Context(), req)
	repo, ok := ctx.Value(repoContextKey{}).(string)
	require.True(t, ok)
	assert.Equal(t, "https://github.com/org/repo", repo)
}

func TestExtractRepoFromContext_NoParam(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp", nil)
	ctx := hostedContextFromRequest(req.Context(), req)
	_, ok := ctx.Value(repoContextKey{}).(string)
	assert.False(t, ok, "no repo param means no context value")
}

func TestRepoFromContext_Helper(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo", nil)
	ctx := hostedContextFromRequest(req.Context(), req)

	repo, ok := repoFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "https://github.com/org/repo", repo)

	// Without param
	req2 := httptest.NewRequest("POST", "/mcp", nil)
	ctx2 := hostedContextFromRequest(req2.Context(), req2)
	_, ok = repoFromContext(ctx2)
	assert.False(t, ok)
}

func TestExtractSchemaFromContext(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo&schema=go", nil)
	ctx := hostedContextFromRequest(req.Context(), req)

	schema, ok := schemaFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "go", schema)

	repo, ok := repoFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "https://github.com/org/repo", repo)
}

func TestExtractSchemaFromContext_NoSchema(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo", nil)
	ctx := hostedContextFromRequest(req.Context(), req)

	_, ok := schemaFromContext(ctx)
	assert.False(t, ok, "no schema param means no context value")
}

// ---------------------------------------------------------------------------
// hosted mode: resolveSession wiring tests
// ---------------------------------------------------------------------------

func TestHosted_ResolveSession_CreatesWorktree(t *testing.T) {
	repoDir := initTestGitRepo(t)
	r := newGraphRegistry("", nil)

	// Get base clone
	baseDir, err := r.getOrCreateRepoClone("file://" + repoDir)
	require.NoError(t, err)

	// Create worktree like resolveSession would
	wtDir, err := createWorktree(baseDir, "test-session")
	require.NoError(t, err)
	defer func() { _ = removeWorktree(baseDir, wtDir) }()

	assert.DirExists(t, wtDir)
	assert.NotEqual(t, baseDir, wtDir)

	// Worktree should have the committed files
	_, err = os.Stat(filepath.Join(wtDir, "main.go"))
	assert.NoError(t, err)
}

func TestHosted_Cleanup_ReleasesClone(t *testing.T) {
	repoDir := initTestGitRepo(t)
	r := newGraphRegistry("", nil)
	repoURL := "file://" + repoDir

	// Simulate: session connects, gets clone + worktree
	_, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	r.sessionRepos.Store("session-1", repoURL)

	// Simulate disconnect
	if url, ok := r.sessionRepos.LoadAndDelete("session-1"); ok {
		r.releaseRepoClone(url.(string))
	}

	// Clone should still exist (timer hasn't fired)
	v, ok := r.repoClones.Load(repoURL)
	require.True(t, ok)
	rc := v.(*repoClone)
	rc.mu.Lock()
	assert.Equal(t, 0, rc.refCount)
	assert.NotNil(t, rc.cleanupTimer, "should have scheduled cleanup")
	rc.cleanupTimer.Stop() // prevent actual cleanup in test
	rc.mu.Unlock()
}

// ---------------------------------------------------------------------------
// End-to-end hosted flow
// ---------------------------------------------------------------------------

func TestHosted_EndToEnd_SharedClone(t *testing.T) {
	repoDir := initTestGitRepo(t)
	r := newGraphRegistry("", nil)
	repoURL := "file://" + repoDir

	// Session 1 connects
	baseDir1, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	r.sessionRepos.Store("session-1", repoURL)

	// Session 2 connects — same repo, shared base clone
	baseDir2, err := r.getOrCreateRepoClone(repoURL)
	require.NoError(t, err)
	r.sessionRepos.Store("session-2", repoURL)

	assert.Equal(t, baseDir1, baseDir2, "same repo should share base clone")

	// Both disconnect
	if url, ok := r.sessionRepos.LoadAndDelete("session-1"); ok {
		r.releaseRepoClone(url.(string))
	}
	if url, ok := r.sessionRepos.LoadAndDelete("session-2"); ok {
		r.releaseRepoClone(url.(string))
	}

	// Clone still exists (timer hasn't fired yet)
	assert.DirExists(t, baseDir1)

	// Stop timer to prevent cleanup during test
	if v, ok := r.repoClones.Load(repoURL); ok {
		rc := v.(*repoClone)
		rc.mu.Lock()
		if rc.cleanupTimer != nil {
			rc.cleanupTimer.Stop()
		}
		rc.mu.Unlock()
	}
}

func TestHosted_NoRepoParam_NormalPath(t *testing.T) {
	r := newGraphRegistry(".", nil)
	lg := r.getOrCreateGraph(".")
	assert.NotNil(t, lg)
}
