package cmd

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bead mache-2a7605 — write_file used to bundle formatting (gofumpt /
// hclwrite) with validation. These tests pin the new contract:
// validation always runs; formatting is opt-out via format=false.

// buildWriteGraph creates a MemoryStore plus a real source file on disk
// so the splice pipeline (validate → optional format → splice → update)
// has something to write into. Returns the graph, the node path, and
// the source file path.
func buildWriteGraph(t *testing.T) (*graph.MemoryStore, string, string) {
	t.Helper()

	// Source file with deliberately-unformatted Go that gofumpt will
	// rewrite (extra blank lines + tabs vs spaces in the body).
	original := "package main\n\nfunc Hello() {\n\treturn\n}\n"
	srcPath := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(srcPath, []byte(original), 0o644))

	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/Hello"},
	})
	store.AddNode(&graph.Node{
		ID:   "pkg/Hello",
		Mode: 0,
		Origin: &graph.SourceOrigin{
			FilePath:  srcPath,
			StartByte: 14, // start of "func Hello() {"
			EndByte:   uint32(len(original)),
		},
		Data: []byte(original[14:]),
	})
	return store, "pkg/Hello", srcPath
}

func TestWriteFile_FormatTrue_AppliesFormatter(t *testing.T) {
	store, nodePath, _ := buildWriteGraph(t)
	handler := makeWriteFileHandler(store)

	// Syntactically valid Go fragment. The crucial assertion is that
	// FormatApplied is reported true — whether the formatter actually
	// changes the bytes depends on whether gofumpt accepts a fragment,
	// which is brittle; covered separately with HCL.
	content := "func Hello() {\n\treturn\n}\n"
	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    nodePath,
		"content": content,
		// format defaults to true
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Status        string `json:"status"`
		FormatApplied bool   `json:"format_applied"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.True(t, resp.FormatApplied, "format=true (default) must report FormatApplied")
}

// TestWriteFile_FormatTrue_HCLNormalizes uses a Terraform file because
// hclwrite.Format works on fragments and reliably normalizes spacing,
// giving a stable signal for the FormatChanged assertion that gofumpt
// can't provide for partial Go files.
func TestWriteFile_FormatTrue_HCLNormalizes(t *testing.T) {
	original := "resource \"aws_vpc\" \"this\" {\n  cidr_block = \"10.0.0.0/16\"\n}\n"
	srcPath := filepath.Join(t.TempDir(), "main.tf")
	require.NoError(t, os.WriteFile(srcPath, []byte(original), 0o644))

	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{
		ID:       "tf",
		Mode:     fs.ModeDir,
		Children: []string{"tf/vpc"},
	})
	store.AddNode(&graph.Node{
		ID:   "tf/vpc",
		Mode: 0,
		Origin: &graph.SourceOrigin{
			FilePath:  srcPath,
			StartByte: 0,
			EndByte:   uint32(len(original)),
		},
	})

	handler := makeWriteFileHandler(store)

	// Misaligned HCL — hclwrite.Format will normalize the indentation.
	misaligned := "resource \"aws_vpc\" \"this\" {\ncidr_block      =     \"10.0.0.0/16\"\n}\n"
	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    "tf/vpc",
		"content": misaligned,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Status        string `json:"status"`
		FormatApplied bool   `json:"format_applied"`
		FormatChanged bool   `json:"format_changed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.True(t, resp.FormatApplied)
	assert.True(t, resp.FormatChanged, "hclwrite must normalize misaligned terraform")
}

func TestWriteFile_FormatFalse_SplicesVerbatim(t *testing.T) {
	store, nodePath, srcPath := buildWriteGraph(t)
	handler := makeWriteFileHandler(store)

	// Valid Go but with extra spaces gofumpt would normalize. With
	// format=false the bytes must land on disk exactly as written.
	verbatim := "func Hello()  {\n\treturn\n}\n"
	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    nodePath,
		"content": verbatim,
		"format":  false,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Status        string `json:"status"`
		FormatApplied bool   `json:"format_applied"`
		FormatChanged bool   `json:"format_changed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	assert.Equal(t, "ok", resp.Status)
	assert.False(t, resp.FormatApplied, "format=false must skip the formatter")
	assert.False(t, resp.FormatChanged, "format_changed must be false when formatter never ran")

	// On-disk content must contain the verbatim spacing. (We sliced the
	// last node, so the file ends with our exact replacement region.)
	got, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(got), verbatim),
		"source file did not end with the verbatim content; got: %q", string(got))
}

func TestWriteFile_ValidationStillRunsWhenFormatFalse(t *testing.T) {
	store, nodePath, srcPath := buildWriteGraph(t)
	handler := makeWriteFileHandler(store)

	// Syntactically broken Go — missing closing brace. Validation must
	// reject regardless of the format flag.
	broken := "func Hello() { return\n"
	original, err := os.ReadFile(srcPath)
	require.NoError(t, err)

	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    nodePath,
		"content": broken,
		"format":  false,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	assert.Equal(t, "validation_error", resp.Status,
		"broken syntax must be rejected even with format=false")

	// File must be unchanged on disk.
	after, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Equal(t, original, after, "validation failure must not touch the source file")
}

// TestWriteFile_RejectsNonWriteBackerBackend pins the fail-fast contract:
// if the graph backend doesn't implement writeBacker (UpdateNodeContent +
// ShiftOrigins), write_file must refuse the request BEFORE running splice.
// Otherwise the on-disk file is modified, the unchecked type assertion
// panics, and disk and graph end up out of sync.
//
// readOnlyGraph satisfies graph.Graph but not writeBacker, simulating a
// future backend (or a test harness) that exposes nodes with Origin but
// can't persist updates.
func TestWriteFile_RejectsNonWriteBackerBackend(t *testing.T) {
	original := "package main\n\nfunc Hello() {}\n"
	srcPath := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(srcPath, []byte(original), 0o644))

	g := &readOnlyGraph{
		node: &graph.Node{
			ID:   "pkg/Hello",
			Mode: 0,
			Origin: &graph.SourceOrigin{
				FilePath:  srcPath,
				StartByte: 14,
				EndByte:   uint32(len(original)),
			},
		},
	}
	handler := makeWriteFileHandler(g)

	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    "pkg/Hello",
		"content": "func Hello() { return }\n",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError, "non-writeBacker backend must yield an error result")
	assert.Contains(t, resultText(t, result), "does not support write-back")

	// Source file must be untouched — the splice must not have run.
	after, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(after), "splice must not run when backend can't accept the update")
}

// readOnlyGraph satisfies graph.Graph but explicitly omits writeBacker.
// Used by TestWriteFile_RejectsNonWriteBackerBackend to exercise the
// fail-fast path in makeWriteFileHandler.
type readOnlyGraph struct {
	node *graph.Node
}

func (g *readOnlyGraph) GetNode(id string) (*graph.Node, error) {
	if id == g.node.ID {
		return g.node, nil
	}
	return nil, graph.ErrNotFound
}
func (*readOnlyGraph) ListChildren(string) ([]string, error)           { return nil, nil }
func (*readOnlyGraph) ListChildStats(string) ([]graph.NodeStat, error) { return nil, nil }
func (*readOnlyGraph) ReadContent(string, []byte, int64) (int, error)  { return 0, nil }
func (*readOnlyGraph) GetCallers(string) ([]*graph.Node, error)        { return nil, nil }
func (*readOnlyGraph) GetCallees(string) ([]*graph.Node, error)        { return nil, nil }
func (*readOnlyGraph) Invalidate(string)                               {}
func (*readOnlyGraph) Act(string, string, string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

var _ graph.Graph = (*readOnlyGraph)(nil)

func TestWriteFile_FormatChangedFalseWhenAlreadyClean(t *testing.T) {
	// When format=true but the input is already gofumpt-clean,
	// FormatApplied is true (we ran the formatter) but FormatChanged
	// should be false (no actual change).
	store, nodePath, _ := buildWriteGraph(t)
	handler := makeWriteFileHandler(store)

	clean := "func Hello() {\n\treturn\n}\n"
	result, err := handler(context.Background(), makeRequest(map[string]any{
		"path":    nodePath,
		"content": clean,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		FormatApplied bool `json:"format_applied"`
		FormatChanged bool `json:"format_changed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	assert.True(t, resp.FormatApplied)
	assert.False(t, resp.FormatChanged, "clean input must not be reported as format-changed")
}
