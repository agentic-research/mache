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
