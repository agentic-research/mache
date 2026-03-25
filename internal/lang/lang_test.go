package lang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_Has18Languages(t *testing.T) {
	expected := []string{
		"go", "python", "javascript", "typescript", "sql", "terraform",
		"yaml", "rust", "toml", "elixir", "java", "c", "cpp",
		"ruby", "php", "kotlin", "swift", "scala",
	}
	for _, name := range expected {
		l := ForName(name)
		require.NotNil(t, l, "missing language: %s", name)
		assert.Equal(t, name, l.Name)
		assert.NotNil(t, l.Grammar(), "nil grammar for %s", name)
		assert.NotEmpty(t, l.Extensions, "no extensions for %s", name)
	}
}

func TestForExt_AllExtensions(t *testing.T) {
	cases := map[string]string{
		".go": "go", ".py": "python", ".js": "javascript",
		".ts": "typescript", ".tsx": "typescript",
		".sql": "sql", ".tf": "terraform", ".hcl": "terraform",
		".yaml": "yaml", ".yml": "yaml",
		".rs": "rust", ".toml": "toml",
		".ex": "elixir", ".exs": "elixir",
		".java": "java", ".c": "c", ".h": "c",
		".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp",
		".hpp": "cpp", ".hxx": "cpp", ".hh": "cpp",
		".rb": "ruby", ".php": "php",
		".kt": "kotlin", ".kts": "kotlin",
		".swift": "swift", ".scala": "scala", ".sc": "scala",
	}
	for ext, wantName := range cases {
		l := ForExt(ext)
		require.NotNil(t, l, "no language for ext %s", ext)
		assert.Equal(t, wantName, l.Name, "wrong language for ext %s", ext)
	}
}

func TestForExt_CaseInsensitive(t *testing.T) {
	l := ForExt(".Go")
	require.NotNil(t, l)
	assert.Equal(t, "go", l.Name)
}

func TestForExt_Unknown(t *testing.T) {
	assert.Nil(t, ForExt(".xyz"))
	assert.Nil(t, ForExt(".md"))
	assert.Nil(t, ForExt(""))
}

func TestForName_Alias(t *testing.T) {
	l := ForName("hcl")
	require.NotNil(t, l, "ForName(\"hcl\") must return terraform for backward compat")
	assert.Equal(t, "terraform", l.Name)
}

func TestForName_Unknown(t *testing.T) {
	assert.Nil(t, ForName("brainfuck"))
}

func TestIsSourceExt(t *testing.T) {
	assert.True(t, IsSourceExt(".go"))
	assert.True(t, IsSourceExt(".java"))
	assert.True(t, IsSourceExt(".swift"))
	assert.True(t, IsSourceExt(".json")) // special case: data format
	assert.False(t, IsSourceExt(".md"))
	assert.False(t, IsSourceExt(".o"))
}

func TestExtensions_ReturnsAll(t *testing.T) {
	exts := Extensions()
	assert.Contains(t, exts, ".go")
	assert.Contains(t, exts, ".scala")
	assert.Contains(t, exts, ".hh")
	assert.Contains(t, exts, ".json")
	assert.True(t, len(exts) >= 28, "expected at least 28 extensions, got %d", len(exts))
}

func TestExtensions_Sorted(t *testing.T) {
	exts := Extensions()
	for i := 1; i < len(exts); i++ {
		assert.True(t, exts[i-1] <= exts[i], "not sorted: %s > %s", exts[i-1], exts[i])
	}
}

func TestForPath(t *testing.T) {
	l := ForPath("/foo/bar/main.go")
	require.NotNil(t, l)
	assert.Equal(t, "go", l.Name)

	assert.Nil(t, ForPath("/foo/README.md"))
}

func TestNoDuplicateExtensions(t *testing.T) {
	seen := map[string]string{}
	for _, l := range Registry {
		for _, ext := range l.Extensions {
			if prev, ok := seen[ext]; ok {
				t.Errorf("extension %s claimed by both %s and %s", ext, prev, l.Name)
			}
			seen[ext] = l.Name
		}
	}
}

func TestEnrichNode_Terraform(t *testing.T) {
	l := ForName("terraform")
	require.NotNil(t, l)
	assert.NotNil(t, l.EnrichNode, "terraform should have EnrichNode set")
}

func TestEnrichNode_NilForMost(t *testing.T) {
	for _, name := range []string{"go", "python", "rust", "java"} {
		l := ForName(name)
		require.NotNil(t, l)
		assert.Nil(t, l.EnrichNode, "%s should not have EnrichNode", name)
	}
}
