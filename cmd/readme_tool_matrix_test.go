package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestREADMEToolMatrixMatchesRegistry pins the README's "what it gives an
// agent" matrix to the live MCP registry.
//
// This exists because the claim drifted in exactly the way you'd expect a
// hand-copied number to: the README prose said "Eighteen MCP tools" while
// the Status table two sections down still said "17 tools", and both were
// maintained by hand against a registry that had moved. The repo already
// has a `drift_doc_outdated_count` smell rule whose own description names
// "'17 MCP tools' should match a SQL ground-truth query" as its motivating
// example — but that rule is a v1 placeholder that returns zero rows, so
// nothing actually caught it.
//
// Ground truth here is the registry itself (via `tools/list` on a real
// server), not a grep over source, so a tool that is renamed or added
// fails this test until the README's matrix is updated to match.
func TestREADMEToolMatrixMatchesRegistry(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(testutil.MacheRepoRoot(t), "README.md"))
	require.NoError(t, err, "read README.md")
	doc := string(readme)

	tools := inspectRegisteredToolsForInvariants(t).Result.Tools

	// Every registered tool must be documented in the README, as inline
	// code (`get_overview`) so it lands in the matrix rather than in
	// incidental prose.
	var undocumented []string
	for _, tool := range tools {
		if !strings.Contains(doc, "`"+tool.Name+"`") {
			undocumented = append(undocumented, tool.Name)
		}
	}
	assert.Empty(t, undocumented,
		"MCP tools registered but absent from README.md — add them to the "+
			"'Code intelligence: what it gives an agent' matrix")

	// The headline count must match the registry. Exact-string containment
	// rather than a regex over prose: the README states the number in one
	// canonical place and this asserts that place is right.
	claim := fmt.Sprintf("**%d MCP tools**", len(tools))
	assert.Contains(t, doc, claim,
		"README.md must state the tool count as %q (registry currently "+
			"registers %d tools); update the matrix intro when tools are "+
			"added or removed", claim, len(tools))
}
