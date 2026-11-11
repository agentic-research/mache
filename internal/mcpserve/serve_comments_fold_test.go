package mcpserve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/mcpharness"
)

// From projection-v13 (leyline v0.29.0) every comment is a nodes row, which
// the raw projection would list as comment, comment_0, line_comment_1 ...
// beside the code. mache folds them (owner decision on mache-1e3d11): no
// listing or overview shows a comment, and no path reaches one.
func TestServe_CommentsAreFolded(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	mcpharness.IsolatedHome(t)
	root := t.TempDir()
	for name, body := range map[string]string{
		"a.go":   "// Package a.\npackage a\n\n// F is documented.\nfunc F() {\n\t// inside\n\tG()\n}\n\n/* block */\nfunc G() {}\n// trailing\n",
		"lib.rs": "/// Doc.\npub fn f() {\n    // inside\n    g();\n}\n// trailing\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o644))
	}
	s := mcpharness.InProcess(t, serveServer(t), mcpharness.Behavior{Roots: []string{root}})

	var listing []struct {
		Name string `json:"name"`
	}
	for _, dir := range []string{"a.go", "a.go/function_declaration_0", "a.go/function_declaration_0/block", "lib.rs", "lib.rs/function_item/block"} {
		listing = nil
		require.NoError(t, json.Unmarshal([]byte(s.Text("list_directory", map[string]any{"path": dir})), &listing), dir)
		for _, e := range listing {
			assert.NotContains(t, e.Name, "comment", "%s lists a comment", dir)
		}
	}
	listing = nil
	require.NoError(t, json.Unmarshal([]byte(s.Text("list_directory", map[string]any{"path": "a.go/function_declaration_0/block"})), &listing))
	require.NotEmpty(t, listing, "a body lists its code")
	listing = nil
	require.NoError(t, json.Unmarshal([]byte(s.Text("list_directory", map[string]any{"path": "a.go"})), &listing))
	var names []string
	for _, e := range listing {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"function_declaration_0", "function_declaration_1", "package_clause"}, names,
		"a.go is its code, named as without comments")

	var ov struct {
		TopLevel []struct {
			Name     string `json:"name"`
			Children int    `json:"children"`
		} `json:"top_level"`
	}
	require.NoError(t, json.Unmarshal([]byte(s.Text("get_overview", nil)), &ov))
	children := map[string]int{}
	for _, e := range ov.TopLevel {
		children[e.Name] = e.Children
	}
	assert.Equal(t, map[string]int{"a.go": 3, "lib.rs": 1}, children, "get_overview counts code children only")

	for _, p := range []string{"a.go/comment_0", "a.go/comment_1", "lib.rs/line_comment_0", "a.go/function_declaration_0/block/comment"} {
		text, isErr := s.Call("read_file", map[string]any{"path": p})
		assert.True(t, isErr || !strings.Contains(text, "//"), "%s must not resolve to a comment: %s", p, text)
	}
}
