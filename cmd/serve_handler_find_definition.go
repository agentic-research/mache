package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// minFuzzyLen guards the partial-substring branch. Symbols shorter than
// this trigger fuzzy matches against essentially every common token in
// large codebases, which is exactly the noise mache-nmia complained
// about. 4 chars is the smallest length where substring matching is
// usually meaningful (e.g. "auth" → AuthZ, OAuth, authenticate).
const minFuzzyLen = 4

// maxFuzzySuggestions caps how many partial-match suggestions ride back
// in a single response. The prior limit was 20 — still a wall of text in
// monorepos. 8 keeps the suggestion list scannable.
const maxFuzzySuggestions = 8

func makeFindDefinitionHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}
		fuzzy := request.GetBool("fuzzy", false)

		// 1. Anchored exact match — case-sensitive. Use LookupDef
		// when the backend supports it: O(1) map lookup, no
		// O(N) snapshot copy of the entire defs map. Falls back
		// to DefsMap for backends that haven't implemented the
		// optional lookup method.
		if dl, ok := g.(defsLookuper); ok {
			if dirIDs := dl.LookupDef(symbol); len(dirIDs) > 0 {
				if r := findDefinitionResultScoped(g, symbol, dirIDs); r != nil {
					return r, nil
				}
				return findDefinitionResult(symbol, dirIDs), nil
			}
		}

		// Backends that ONLY expose DefsMap (older/composite shapes)
		// or that didn't hit on the LookupDef path fall through to
		// the snapshot for the case-insensitive + fuzzy paths below.
		dp := g.(defsMapProvider)
		defs := dp.DefsMap()

		// 1b. Anchored exact via the snapshot — only reached when
		// the backend lacks a defsLookuper. Mirrors the old behavior.
		if _, hasLookup := g.(defsLookuper); !hasLookup {
			if dirIDs, ok := defs[symbol]; ok {
				if r := findDefinitionResultScoped(g, symbol, dirIDs); r != nil {
					return r, nil
				}
				return findDefinitionResult(symbol, dirIDs), nil
			}
		}

		// 2. Anchored exact match — case-insensitive.
		// Same token, just different case — still "anchored," not noise.
		symbolLower := strings.ToLower(symbol)
		for token, ids := range defs {
			if strings.ToLower(token) == symbolLower {
				if r := findDefinitionResultScoped(g, symbol, ids); r != nil {
					return r, nil
				}
				return findDefinitionResult(symbol, ids), nil
			}
		}

		// 3. Fuzzy substring fallback — opt-in only. The default behavior
		// previously returned a wall of partial matches that drowned out
		// the right answer in monorepos (bead mache-nmia).
		if fuzzy && len(symbol) >= minFuzzyLen {
			matches := collectFuzzyMatches(defs, symbolLower, maxFuzzySuggestions)
			if len(matches) > 0 {
				type suggestion struct {
					Message     string   `json:"message"`
					Suggestions []string `json:"suggestions"`
				}
				data, _ := json.MarshalIndent(suggestion{
					Message:     fmt.Sprintf("no exact definition for %q, but found similar symbols (fuzzy=true)", symbol),
					Suggestions: matches,
				}, "", "  ")
				return mcp.NewToolResultText(string(data)), nil
			}
		}

		// 4. LSP fallback: try _lsp_defs from a ley-line pre-baked DB.
		if qg, ok := g.(refsQuerier); ok {
			lspDefs, err := queryLSPDefs(qg, symbol)
			if err == nil && len(lspDefs) > 0 {
				type lspResult struct {
					Symbol      string           `json:"symbol"`
					Source      string           `json:"source"`
					Definitions []lspDefLocation `json:"definitions"`
				}
				data, _ := json.MarshalIndent(lspResult{
					Symbol:      symbol,
					Source:      "lsp",
					Definitions: lspDefs,
				}, "", "  ")
				return mcp.NewToolResultText(string(data)), nil
			}
		}

		// 5. Nothing matched. Hint at fuzzy if available.
		hint := ""
		if !fuzzy && len(symbol) >= minFuzzyLen {
			hint = " — try fuzzy=true for substring matches"
		}
		if serveControl != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no definition found for %q — daemon may still be parsing, retry shortly%s", symbol, hint)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("no definition found for %q%s", symbol, hint)), nil
	}
}

// findDefinitionResult shapes the success response for find_definition.
func findDefinitionResult(symbol string, dirIDs []string) *mcp.CallToolResult {
	type defResult struct {
		Symbol      string   `json:"symbol"`
		Definitions []string `json:"definitions"`
	}
	data, _ := json.MarshalIndent(defResult{Symbol: symbol, Definitions: dirIDs}, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// findDefinitionResultScoped is the cross-repo variant: when the
// active graph is a CompositeGraph and at least one definition
// carries a mount prefix, emit the {symbol, definitions: [{path,
// mount}]} shape so agents see which mount each def came from.
// Returns nil if annotation adds nothing — caller falls through to
// findDefinitionResult.
func findDefinitionResultScoped(g graph.Graph, symbol string, dirIDs []string) *mcp.CallToolResult {
	scoped := annotateMounts(g, dirIDs)
	if scoped == nil {
		return nil
	}
	type defResult struct {
		Symbol      string       `json:"symbol"`
		Definitions []scopedItem `json:"definitions"`
	}
	data, _ := json.MarshalIndent(defResult{Symbol: symbol, Definitions: scoped}, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// collectFuzzyMatches walks the defs map looking for tokens whose
// case-insensitive form contains symbolLower (or vice versa) and returns
// up to `limit` "token → dir_id" pairs. Caller has already ensured
// symbolLower is at least minFuzzyLen.
func collectFuzzyMatches(defs map[string][]string, symbolLower string, limit int) []string {
	var matches []string
	for token, ids := range defs {
		if len(matches) >= limit {
			break
		}
		tokenLower := strings.ToLower(token)
		if !strings.Contains(tokenLower, symbolLower) && !strings.Contains(symbolLower, tokenLower) {
			continue
		}
		for _, id := range ids {
			matches = append(matches, token+" → "+id)
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches
}
