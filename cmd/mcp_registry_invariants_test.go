package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// inspectedTools is the minimal shape we need from the MCP
// `tools/list` JSON-RPC response — name + the input schema's
// property names — to assert structural invariants. We marshal +
// unmarshal the response so the test is decoupled from any
// internal mcp-go types.
type inspectedTools struct {
	Result struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	} `json:"result"`
}

// inspectRegisteredToolsForInvariants spins up a minimal MCPServer, attaches the
// production tool registrations via registerMCPTools, calls
// `tools/list`, and returns the inspected tool inventory.
func inspectRegisteredToolsForInvariants(t *testing.T) inspectedTools {
	t.Helper()
	// WithToolCapabilities(false) matches the production server
	// (cmd/serve.go) and other tool-listing tests (cmd/serve_test.go),
	// keeping the tools/list response shape stable across mcp-go
	// version drift. Flagged by Copilot review on PR #438.
	s := server.NewMCPServer("mache-registry-test", "test", server.WithToolCapabilities(false))
	r := newGraphRegistry(".", nil)
	registerMCPTools(s, r)

	resp := s.HandleMessage(context.Background(), json.RawMessage(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "tools/list",
		"params": {}
	}`))
	if resp == nil {
		t.Fatal("tools/list returned nil response")
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var parsed inspectedTools
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, body)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Fatalf("tools/list returned no tools (body=%s)", body)
	}
	return parsed
}

// TestMCPRegistry_NoLineOrColumnParams enforces the bead-7d321e
// invariant: agents emit wrong line/col routinely (the load-bearing
// cclsp design lesson), so mache's MCP tool surface must NEVER
// expose tools whose params are line- or column-typed.
//
// Any future tool that grows a `line`, `column`, `lineno`, or `col`
// param fails this test. Position info must be resolved internally
// from symbol_name + symbol_kind.
//
// Falsified by: someone adding a position-based tool param.
// Stays green by: keeping the symbol-aware contract.
func TestMCPRegistry_NoLineOrColumnParams(t *testing.T) {
	tools := inspectRegisteredToolsForInvariants(t)

	bannedParamNames := map[string]bool{
		"line":   true,
		"col":    true,
		"column": true,
		"lineno": true,
		"colno":  true,
	}

	var violations []string
	for _, tool := range tools.Result.Tools {
		for paramName := range tool.InputSchema.Properties {
			if bannedParamNames[strings.ToLower(paramName)] {
				violations = append(violations, tool.Name+"."+paramName)
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("MCP tools must not expose line/col-typed params (bead mache-7d321e). Violations: %v\n"+
			"Use symbol_name + symbol_kind instead; resolve position internally. "+
			"This invariant matches the cclsp pattern that agents emit wrong line/col routinely.",
			violations)
	}
}

// TestMCPRegistry_SymbolAwareToolsHaveKind asserts that every tool
// whose params include `symbol` or `token` also exposes a `kind`
// param, so agents can disambiguate same-named-different-kind cases.
// Tools that legitimately don't need kind (e.g. semantic_search,
// which is NL-based and pre-disambiguates internally) grandfather
// in via the explicit exemption list.
func TestMCPRegistry_SymbolAwareToolsHaveKind(t *testing.T) {
	tools := inspectRegisteredToolsForInvariants(t)

	// Tools that take a symbol/token but legitimately don't need
	// kind. Add here only with rationale; default is to require kind.
	exempt := map[string]string{
		// semantic_search uses NL query, not a specific symbol name,
		// so kind ambiguity doesn't apply the same way.
		"semantic_search": "NL query; not a name lookup",
		// search uses pattern matching with a separate `type` filter
		// that already covers the kind-like discrimination.
		"search": "pattern-based; has `type` param for kind-like filtering",
		// resolve_ref takes a typed cross-language ref token (e.g.
		// `mod:./modules/vpc`); the scheme prefix already encodes the
		// reference kind, distinct from construct kind.
		"resolve_ref": "typed cross-language ref token; scheme prefix encodes kind",
	}

	var missing []string
	for _, tool := range tools.Result.Tools {
		props := tool.InputSchema.Properties
		_, hasSymbol := props["symbol"]
		_, hasToken := props["token"]
		if !hasSymbol && !hasToken {
			continue
		}
		if _, isExempt := exempt[tool.Name]; isExempt {
			continue
		}
		if _, hasKind := props["kind"]; !hasKind {
			missing = append(missing, tool.Name)
		}
	}

	if len(missing) > 0 {
		t.Errorf("symbol-aware tools must expose a `kind` param for disambiguation (bead mache-7d321e). Missing: %v\n"+
			"Add `mcp.WithString(\"kind\", mcp.Description(...))` to the tool registration "+
			"and route through filterDirIDsByKind in the handler.\n"+
			"If a tool legitimately doesn't need kind, add it to the exempt map in this test with a rationale.",
			missing)
	}
}
