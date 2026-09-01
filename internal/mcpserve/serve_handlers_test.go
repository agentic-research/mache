package mcpserve

import (
	"sort"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/mcpregistry"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
)

// TestRegisterMCPToolsMatchesMcpRegistry is the drift gate between
// cmd/serve_handlers.go (the runtime registration) and
// internal/mcpregistry.ToolRegistry() (the canonical list consumed by
// tools/server-json-gen).
//
// The two lists MUST stay in lock-step: every tool registered with the
// mcp-go server must appear in the registry, and every name in the
// registry must be registered. Adding a tool in one place without the
// other is the failure mode this gate exists to catch.
//
// See bead `mache-802d2b` — the Phase 4b rollout depends on a stable
// server.json artifact, and a hand-maintained parallel list would rot
// silently.
func TestRegisterMCPToolsMatchesMcpRegistry(t *testing.T) {
	// Use a tiny in-memory graph fixture — registerMCPTools needs a
	// graphRegistry but only to wire handler closures; tools/list
	// doesn't invoke any of them.
	store := graph.NewMemoryStore()
	registry := newGraphRegistry(".", nil)
	registry.graphs.Store(".", &lazyGraph{inner: store})

	s := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false))
	registerMCPTools(s, registry)

	registered := listRegisteredTools(t, s)

	expected := map[string]bool{}
	for _, tool := range mcpregistry.ToolRegistry() {
		expected[tool.Name] = true
	}

	// Symmetric difference reporting — easier to read on failure than
	// "expected X but got Y" of one direction at a time.
	var missingFromHandlers, missingFromRegistry []string
	for name := range expected {
		if !registered[name] {
			missingFromHandlers = append(missingFromHandlers, name)
		}
	}
	for name := range registered {
		if !expected[name] {
			missingFromRegistry = append(missingFromRegistry, name)
		}
	}
	sort.Strings(missingFromHandlers)
	sort.Strings(missingFromRegistry)

	assert.Empty(t, missingFromHandlers,
		"tools in internal/mcpregistry but NOT registered in cmd/serve_handlers.go — add s.AddTool() calls for them")
	assert.Empty(t, missingFromRegistry,
		"tools registered in cmd/serve_handlers.go but NOT in internal/mcpregistry — add them to ToolRegistry() and claim them in a cloister group")
}
