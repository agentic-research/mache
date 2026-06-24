package cmd

import (
	"sort"
	"strings"
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
