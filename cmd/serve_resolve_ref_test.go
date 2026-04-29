package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests cover the resolveModScheme branch directly so we don't need an
// MCP transport spun up — the handler is a thin JSON wrapper.

func TestResolveModScheme_LocalDir(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "modules", "vpc")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte("resource \"aws_vpc\" \"this\" {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "outputs.tf"), []byte("output \"vpc_id\" {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "README.md"), []byte("# VPC"), 0o644))

	// base_path is a terraform file in the project root.
	basePath := filepath.Join(root, "main.tf")
	require.NoError(t, os.WriteFile(basePath, []byte("module \"vpc\" { source = \"./modules/vpc\" }"), 0o644))

	got := resolveModScheme("./modules/vpc", basePath)
	assert.Equal(t, "mod", got.Scheme)
	assert.Equal(t, "./modules/vpc", got.Locator)
	assert.True(t, got.Exists, "module dir should exist")
	assert.True(t, got.IsDir)
	assert.Empty(t, got.Error)
	assert.Empty(t, got.RemoteHint)

	resolved, err := filepath.EvalSymlinks(got.Resolved)
	require.NoError(t, err)
	expected, err := filepath.EvalSymlinks(moduleDir)
	require.NoError(t, err)
	assert.Equal(t, expected, resolved)

	// Files should include both .tf entries with terraform language
	// detected, plus the README.md without a lang tag.
	names := map[string]resolveRefEntry{}
	for _, f := range got.Files {
		names[f.Name] = f
	}
	require.Contains(t, names, "main.tf")
	require.Contains(t, names, "outputs.tf")
	require.Contains(t, names, "README.md")
	assert.Equal(t, "terraform", names["main.tf"].Lang)
	assert.Equal(t, "terraform", names["outputs.tf"].Lang)
}

func TestResolveModScheme_LocalDirRelativeToDir(t *testing.T) {
	// Same as above but base_path is a directory rather than a file —
	// the resolver should still anchor relative locators against it.
	root := t.TempDir()
	moduleDir := filepath.Join(root, "modules", "vpc")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	got := resolveModScheme("./modules/vpc", root)
	assert.True(t, got.Exists)
	assert.True(t, got.IsDir)
	assert.Empty(t, got.Error)
}

func TestResolveModScheme_RemoteLocatorReturnsHint(t *testing.T) {
	got := resolveModScheme("github.com/foo/bar", t.TempDir())
	assert.Equal(t, "github.com/foo/bar", got.Locator)
	assert.False(t, got.Exists)
	assert.NotEmpty(t, got.RemoteHint, "remote locator must surface a hint")
	assert.Empty(t, got.Resolved)
	assert.Empty(t, got.Files)
}

func TestResolveModScheme_LocalLocatorRequiresBasePath(t *testing.T) {
	got := resolveModScheme("./modules/vpc", "")
	assert.Empty(t, got.Resolved)
	assert.NotEmpty(t, got.Error)
	assert.Contains(t, got.Error, "base_path")
}

func TestResolveModScheme_TargetMissing(t *testing.T) {
	got := resolveModScheme("./does/not/exist", t.TempDir())
	assert.False(t, got.Exists)
	assert.NotEmpty(t, got.Error, "missing target must surface an error")
	assert.NotEmpty(t, got.Resolved, "Resolved should still be set so the caller can see what was tried")
}

func TestResolveModScheme_DotSlashFile(t *testing.T) {
	// When the locator points at a single file (not a directory), Files is
	// empty and IsDir is false.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "shared.tf"), []byte("variable \"x\" {}"), 0o644))

	got := resolveModScheme("./shared.tf", root)
	assert.True(t, got.Exists)
	assert.False(t, got.IsDir)
	assert.Empty(t, got.Files)
}

func TestResolveResponseSerializesCleanly(t *testing.T) {
	// JSON roundtrip catches struct-tag typos and exported-field issues.
	resp := resolveRefResponse{
		Scheme:   "mod",
		Locator:  "./vpc",
		Resolved: "/abs/vpc",
		Exists:   true,
		IsDir:    true,
		Files: []resolveRefEntry{
			{Name: "main.tf", IsDir: false, Size: 42, Lang: "terraform"},
			{Name: "subdir", IsDir: true},
		},
	}
	b, err := json.Marshal(resp)
	require.NoError(t, err)

	var back resolveRefResponse
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, resp, back)
}

func TestIsLocalRelativeLocator(t *testing.T) {
	cases := map[string]bool{
		"./modules/vpc":    true,
		"../shared":        true,
		"github.com/x/y":   false,
		"foo/bar":          false,
		"":                 false,
		"git::https://...": false,
		"./":               true,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, isLocalRelativeLocator(in))
		})
	}
}
