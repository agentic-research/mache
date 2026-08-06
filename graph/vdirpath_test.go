package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCallersPath(t *testing.T) {
	assert.True(t, IsCallersPath("/funcs/Foo/callers"))
	assert.True(t, IsCallersPath("/funcs/Foo/callers/funcs_Bar_source"))
	assert.True(t, IsCallersPath("/callers/entry"))
	assert.False(t, IsCallersPath("/funcs/Foo"))
	assert.False(t, IsCallersPath("/funcs/Foo/callees"))
	assert.False(t, IsCallersPath(""))
}

func TestParseCallersPath(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantDir   string
		wantEntry string
	}{
		{"standard", "/funcs/Foo/callers/funcs_Bar_source", "/funcs/Foo", "funcs_Bar_source"},
		{"dir only", "/funcs/Foo/callers", "/funcs/Foo", ""},
		{"trailing slash", "/funcs/Foo/callers/", "/funcs/Foo", ""},
		{"root level", "/callers/entry", "/", "entry"},
		{"root dir only", "/callers", "/", ""},
		{"not callers", "/funcs/Foo/source", "", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, entry := ParseCallersPath(tt.path)
			assert.Equal(t, tt.wantDir, dir)
			assert.Equal(t, tt.wantEntry, entry)
		})
	}
}

func TestIsCalleesPath(t *testing.T) {
	assert.True(t, IsCalleesPath("/funcs/Foo/callees"))
	assert.True(t, IsCalleesPath("/funcs/Foo/callees/funcs_Bar_source"))
	assert.False(t, IsCalleesPath("/funcs/Foo/callers"))
	assert.False(t, IsCalleesPath("/funcs/Foo"))
	assert.False(t, IsCalleesPath(""))
}

func TestParseCalleesPath(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantDir   string
		wantEntry string
	}{
		{"standard", "/funcs/Foo/callees/funcs_Bar_source", "/funcs/Foo", "funcs_Bar_source"},
		{"dir only", "/funcs/Foo/callees", "/funcs/Foo", ""},
		{"trailing slash", "/funcs/Foo/callees/", "/funcs/Foo", ""},
		{"root level", "/callees/entry", "/", "entry"},
		{"not callees", "/funcs/Foo/callers/entry", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, entry := ParseCalleesPath(tt.path)
			assert.Equal(t, tt.wantDir, dir)
			assert.Equal(t, tt.wantEntry, entry)
		})
	}
}

func TestIsDiagPath(t *testing.T) {
	assert.True(t, IsDiagPath("/funcs/Foo/_diagnostics"))
	assert.True(t, IsDiagPath("/funcs/Foo/_diagnostics/last-write-status"))
	assert.True(t, IsDiagPath("/_diagnostics/ast-errors"))
	assert.False(t, IsDiagPath("/funcs/Foo"))
	assert.False(t, IsDiagPath("/funcs/Foo/diagnostics")) // no underscore
	assert.False(t, IsDiagPath(""))
}

func TestParseDiagPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantDir  string
		wantFile string
	}{
		{"standard", "/funcs/Foo/_diagnostics/last-write-status", "/funcs/Foo", "last-write-status"},
		{"ast-errors", "/funcs/Foo/_diagnostics/ast-errors", "/funcs/Foo", "ast-errors"},
		{"dir only", "/funcs/Foo/_diagnostics", "/funcs/Foo", ""},
		{"trailing slash", "/funcs/Foo/_diagnostics/", "/funcs/Foo", ""},
		{"root level", "/_diagnostics/lint", "/", "lint"},
		{"not diag", "/funcs/Foo/source", "", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, file := ParseDiagPath(tt.path)
			assert.Equal(t, tt.wantDir, dir)
			assert.Equal(t, tt.wantFile, file)
		})
	}
}

func TestFindSourceChild(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{
		ID:       "pkg/Foo",
		Mode:     0o755 | 1<<31, // dir
		Children: []string{"pkg/Foo/source", "pkg/Foo/doc"},
	})
	store.AddNode(&Node{ID: "pkg/Foo/source", Mode: 0o444, Data: []byte("code")})
	store.AddNode(&Node{ID: "pkg/Foo/doc", Mode: 0o444, Data: []byte("docs")})

	assert.Equal(t, "pkg/Foo/source", FindSourceChild(store, "pkg/Foo"))
}

func TestFindSourceChild_NotFound(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{
		ID:       "pkg/Bar",
		Mode:     0o755 | 1<<31,
		Children: []string{"pkg/Bar/doc"},
	})
	store.AddNode(&Node{ID: "pkg/Bar/doc", Mode: 0o444})

	assert.Equal(t, "", FindSourceChild(store, "pkg/Bar"))
}

func TestFindSourceChild_NonexistentDir(t *testing.T) {
	store := NewMemoryStore()
	assert.Equal(t, "", FindSourceChild(store, "nonexistent"))
}

func TestFindSourceChild_BareChildName(t *testing.T) {
	// When ListChildren returns bare names (nodes-table path), FindSourceChild
	// should prepend the dir ID.
	store := NewMemoryStore()
	store.AddNode(&Node{
		ID:       "pkg/Baz",
		Mode:     0o755 | 1<<31,
		Children: []string{"source"}, // bare name, no slash
	})
	store.AddNode(&Node{ID: "source", Mode: 0o444})

	assert.Equal(t, "pkg/Baz/source", FindSourceChild(store, "pkg/Baz"))
}

func TestVDirSymlinkTarget(t *testing.T) {
	// /funcs/Foo → depth 2, +1 for virtual dir = 3 levels of ../
	target := VDirSymlinkTarget("/funcs/Foo", "funcs/Bar/source")
	assert.Equal(t, "../../../funcs/Bar/source", target)

	// / → depth 1, +1 = 2
	target = VDirSymlinkTarget("/", "funcs/Foo/source")
	assert.Equal(t, "../../funcs/Foo/source", target)

	// deeper path
	target = VDirSymlinkTarget("/a/b/c/d", "x/y")
	assert.Equal(t, "../../../../../x/y", target)
}
