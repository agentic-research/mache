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

func TestRepoFromContext_Helper(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp?repo=https://github.com/org/repo", nil)
	ctx := repoContextFromRequest(req.Context(), req)

	repo, ok := repoFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "https://github.com/org/repo", repo)

	// Without param
	req2 := httptest.NewRequest("POST", "/mcp", nil)
	ctx2 := repoContextFromRequest(req2.Context(), req2)
	_, ok = repoFromContext(ctx2)
	assert.False(t, ok)
}
