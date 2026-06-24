package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Known construct kinds mache projects today. The path layout (per
// cmd/agent.go's user-facing tour) puts construct directories at
// `{pkg}/<kind-plural>/<symbol>/...` — `functions/`, `methods/`,
// `types/`, `constants/`, `variables/`, `imports/`. The MCP API
// accepts the singular form (matching LSP-style kind names) and we
// translate to the plural directory segment here.
var kindToPathSegment = map[string]string{
	"function": "/functions/",
	"method":   "/methods/",
	"type":     "/types/",
	"constant": "/constants/",
	"variable": "/variables/",
	"import":   "/imports/",
}

// supportedKinds returns the singular kind names accepted by mache's
// MCP tools. The slice is sorted so error messages and tool
// documentation produce stable output (map iteration is otherwise
// nondeterministic — flagged by Copilot review on PR #438).
func supportedKinds() []string {
	out := make([]string, 0, len(kindToPathSegment))
	for k := range kindToPathSegment {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// filterDirIDsByKind keeps only dirIDs whose construct path segment
// matches the requested kind. An empty `kind` is the no-op identity —
// caller is responsible for not invoking this on inputs they expect
// to be returned verbatim. An unknown `kind` returns nil and `false`
// so the handler can surface a precise error rather than silently
// returning empty results.
func filterDirIDsByKind(dirIDs []string, kind string) ([]string, bool) {
	if kind == "" {
		return dirIDs, true
	}
	segment, ok := kindToPathSegment[kind]
	if !ok {
		return nil, false
	}
	filtered := make([]string, 0, len(dirIDs))
	for _, id := range dirIDs {
		if strings.Contains(id, segment) {
			filtered = append(filtered, id)
		}
	}
	return filtered, true
}

// validateKindParam pulls the optional `kind` param from an MCP
// request and validates it against the supported kind set. Returns
// either (kind, nil) on success or ("", errResult) when the kind
// is unknown. Caller propagates errResult and returns immediately
// when non-nil. Extracted from per-handler boilerplate to keep
// each handler's fan-out under the find_smells threshold (Copilot
// review on PR #438, fan_out_skew advisory).
func validateKindParam(request mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	kind := request.GetString("kind", "")
	if _, ok := filterDirIDsByKind(nil, kind); !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"unknown kind %q — accepted values: %s",
			kind, strings.Join(supportedKinds(), ", ")))
	}
	return kind, nil
}

// filterByNodeIDKind keeps only items whose NodeID (extracted via
// idOf) matches the requested kind. Generic over the item type
// because lspDefLocation, lspRefLocation, and hover/diag result
// shapes all carry a NodeID field but have distinct struct types.
// Returns items unchanged when kind == "". Extracted from the
// repeated build-IDs / build-keep-map / filter-results loop that
// appeared 4 times across serve_lsp.go + serve_handler_*.go
// (Copilot fan_out_skew advisory).
func filterByNodeIDKind[T any](items []T, kind string, idOf func(T) string) []T {
	if kind == "" {
		return items
	}
	ids := make([]string, len(items))
	for i, x := range items {
		ids[i] = idOf(x)
	}
	filteredIDs, _ := filterDirIDsByKind(ids, kind)
	keep := make(map[string]bool, len(filteredIDs))
	for _, id := range filteredIDs {
		keep[id] = true
	}
	kept := items[:0]
	for _, x := range items {
		if keep[idOf(x)] {
			kept = append(kept, x)
		}
	}
	return kept
}
