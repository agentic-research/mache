package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func makeGetTypeInfoHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}

		qg := g.(refsQuerier)

		// Check if _lsp_hover table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp_hover'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available — run `leyline lsp` to enrich the database"), nil
		}
		var tableExists int
		if rows.Next() {
			_ = rows.Scan(&tableExists)
		}
		_ = rows.Close()
		if tableExists == 0 {
			// Auto-enrichment: if file param is provided, trigger LSP via ley-line daemon
			filePath := request.GetString("file", "")
			if filePath == "" {
				return mcp.NewToolResultError("no _lsp_hover table — pass 'file' param to auto-enrich or run `leyline lsp`"), nil
			}

			result, err := enrichAndQueryTypeInfo(filePath, symbol)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return mcp.NewToolResultError(fmt.Sprintf("no _lsp_hover table — auto-enrichment failed: %v", err)), nil
			}
			return result, nil
		}

		// LSP tables exist in mache's graph — query directly
		return queryTypeInfoFromGraph(qg, symbol)
	}
}

func makeGetDiagnosticsHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		limit := request.GetInt("limit", 50)

		qg := g.(refsQuerier)

		// Check if _lsp table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available — run `leyline lsp` to enrich the database"), nil
		}
		var tableExists int
		if rows.Next() {
			_ = rows.Scan(&tableExists)
		}
		_ = rows.Close()
		if tableExists == 0 {
			filePath := request.GetString("file", "")
			if filePath == "" {
				return mcp.NewToolResultError("no _lsp table — pass 'file' param to auto-enrich or run `leyline lsp`"), nil
			}

			result, err := enrichAndQueryDiagnostics(filePath, symbol, limit)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return mcp.NewToolResultError(fmt.Sprintf("no _lsp table — auto-enrichment failed: %v", err)), nil
			}
			return result, nil
		}

		return queryDiagnosticsFromGraph(qg, symbol, limit)
	}
}

// enrichAndQueryTypeInfo triggers LSP enrichment and queries hover data
// directly from the ley-line daemon's arena via UDS, bypassing mache's
// in-memory graph (which doesn't have _lsp* tables).
// Uses a single connection for both enrichment and query.
func enrichAndQueryTypeInfo(filePath, symbol string) (*mcp.CallToolResult, error) {
	sockPath, err := leyline.DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("discover/start leyline: %w", err)
	}
	client, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Enrichment phase — 30s timeout for LSP
	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Tool("lsp", map[string]any{"file": filePath})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)

	// Query phase — reuse same connection, reset deadline
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, fmt.Errorf("set query deadline: %w", err)
	}

	escaped := escapeLikePattern(symbol)
	rows, err := client.Query(fmt.Sprintf(
		`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE '%%%s' ESCAPE '\'`, escaped))
	if err != nil {
		return nil, fmt.Errorf("query _lsp_hover via daemon: %w", err)
	}

	type hoverResult struct {
		NodeID    string `json:"node_id"`
		HoverText string `json:"hover_text"`
	}

	var results []hoverResult
	for _, row := range rows {
		if len(row) >= 2 {
			results = append(results, hoverResult{
				NodeID:    fmt.Sprint(row[0]),
				HoverText: fmt.Sprint(row[1]),
			})
		}
	}

	// Fallback: broader match
	if len(results) == 0 {
		rows, err = client.Query(fmt.Sprintf(
			`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE '%%%s%%' ESCAPE '\'`, escaped))
		if err == nil {
			for _, row := range rows {
				if len(row) >= 2 {
					results = append(results, hoverResult{
						NodeID:    fmt.Sprint(row[0]),
						HoverText: fmt.Sprint(row[1]),
					})
				}
			}
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("LSP enrichment completed but no hover info found for %q", symbol)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// queryTypeInfoFromGraph queries _lsp_hover from mache's in-memory graph
// (used when LSP tables already exist in the graph, e.g. from leyline lsp CLI).
func queryTypeInfoFromGraph(qg refsQuerier, symbol string) (*mcp.CallToolResult, error) {
	type hoverResult struct {
		NodeID    string `json:"node_id"`
		HoverText string `json:"hover_text"`
	}

	rows, err := qg.QueryRefs(
		`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE ? ESCAPE '\'`,
		"%/"+escapeLikeMeta(symbol),
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query _lsp_hover: %v", err)), nil
	}

	var results []hoverResult
	for rows.Next() {
		var r hoverResult
		if err := rows.Scan(&r.NodeID, &r.HoverText); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	if len(results) == 0 {
		rows, err = qg.QueryRefs(
			`SELECT node_id, hover_text FROM _lsp_hover WHERE node_id LIKE ? ESCAPE '\'`,
			"%"+escapeLikeMeta(symbol)+"%",
		)
		if err == nil {
			for rows.Next() {
				var r hoverResult
				if err := rows.Scan(&r.NodeID, &r.HoverText); err != nil {
					continue
				}
				results = append(results, r)
			}
			_ = rows.Close()
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no type info found for %q", symbol)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// enrichAndQueryDiagnostics triggers LSP enrichment and queries diagnostics
// directly from the ley-line daemon's arena via UDS.
// Uses a single connection for both enrichment and query.
func enrichAndQueryDiagnostics(filePath, symbol string, limit int) (*mcp.CallToolResult, error) {
	sockPath, err := leyline.DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("discover/start leyline: %w", err)
	}
	client, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Tool("lsp", map[string]any{"file": filePath})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)

	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, fmt.Errorf("set query deadline: %w", err)
	}

	var query string
	if symbol != "" {
		escaped := escapeLikePattern(symbol)
		query = fmt.Sprintf(
			`SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' AND node_id LIKE '%%%s%%' ESCAPE '\' LIMIT %d`,
			escaped, limit)
	} else {
		query = fmt.Sprintf(
			`SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' LIMIT %d`,
			limit)
	}

	rows, err := client.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query _lsp via daemon: %w", err)
	}

	type diagResult struct {
		NodeID      string `json:"node_id"`
		SymbolKind  string `json:"symbol_kind"`
		Diagnostics string `json:"diagnostics"`
	}

	var results []diagResult
	for _, row := range rows {
		if len(row) >= 3 {
			results = append(results, diagResult{
				NodeID:      fmt.Sprint(row[0]),
				SymbolKind:  fmt.Sprint(row[1]),
				Diagnostics: fmt.Sprint(row[2]),
			})
		}
	}

	if len(results) == 0 {
		if symbol != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q", symbol)), nil
		}
		return mcp.NewToolResultText("no diagnostics found"), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// queryDiagnosticsFromGraph queries _lsp from mache's in-memory graph.
func queryDiagnosticsFromGraph(qg refsQuerier, symbol string, limit int) (*mcp.CallToolResult, error) {
	type diagResult struct {
		NodeID      string `json:"node_id"`
		SymbolKind  string `json:"symbol_kind"`
		Diagnostics string `json:"diagnostics"`
	}

	var query string
	var args []any
	if symbol != "" {
		query = `SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' AND node_id LIKE ? ESCAPE '\' LIMIT ?`
		args = []any{"%" + escapeLikeMeta(symbol) + "%", limit}
	} else {
		query = `SELECT node_id, symbol_kind, diagnostics FROM _lsp WHERE diagnostics IS NOT NULL AND diagnostics != '' LIMIT ?`
		args = []any{limit}
	}

	rows, err := qg.QueryRefs(query, args...)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query _lsp: %v", err)), nil
	}

	var results []diagResult
	for rows.Next() {
		var r diagResult
		if err := rows.Scan(&r.NodeID, &r.SymbolKind, &r.Diagnostics); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	if len(results) == 0 {
		if symbol != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q", symbol)), nil
		}
		return mcp.NewToolResultText("no diagnostics found"), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// ---------------------------------------------------------------------------
// LSP definition/reference query helpers
// ---------------------------------------------------------------------------

// lspDefLocation represents a definition location from the _lsp_defs table.
type lspDefLocation struct {
	NodeID    string `json:"node_id"`
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartCol  int    `json:"start_col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
}

// lspRefLocation represents a reference location from the _lsp_refs table.
type lspRefLocation struct {
	NodeID    string `json:"node_id"`
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartCol  int    `json:"start_col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
}

// queryLSPDefs queries the _lsp_defs table for definition locations of a symbol.
// Returns nil, nil if the table does not exist.
func queryLSPDefs(qg refsQuerier, symbol string) ([]lspDefLocation, error) {
	// Check if _lsp_defs table exists
	rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp_defs'`)
	if err != nil {
		return nil, nil
	}
	var tableExists int
	if rows.Next() {
		_ = rows.Scan(&tableExists)
	}
	_ = rows.Close()
	if tableExists == 0 {
		return nil, nil
	}

	// Try node_id suffix match (symbol is the trailing component of node_id)
	escaped := escapeLikeMeta(symbol)
	rows, err = qg.QueryRefs(
		`SELECT node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col
		 FROM _lsp_defs WHERE node_id LIKE ? ESCAPE '\'`,
		"%/"+escaped,
	)
	if err != nil {
		return nil, fmt.Errorf("query _lsp_defs: %w", err)
	}

	var results []lspDefLocation
	for rows.Next() {
		var r lspDefLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	// Fallback: broader LIKE match
	if len(results) == 0 {
		rows, err = qg.QueryRefs(
			`SELECT node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col
			 FROM _lsp_defs WHERE node_id LIKE ? ESCAPE '\'`,
			"%"+escaped+"%",
		)
		if err != nil {
			return nil, fmt.Errorf("query _lsp_defs (broad): %w", err)
		}
		for rows.Next() {
			var r lspDefLocation
			if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
				continue
			}
			results = append(results, r)
		}
		_ = rows.Close()
	}

	return results, nil
}

// queryLSPRefs queries the _lsp_refs table for reference locations of a symbol.
// Returns nil, nil if the table does not exist.
func queryLSPRefs(qg refsQuerier, symbol string) ([]lspRefLocation, error) {
	// Check if _lsp_refs table exists
	rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp_refs'`)
	if err != nil {
		return nil, nil
	}
	var tableExists int
	if rows.Next() {
		_ = rows.Scan(&tableExists)
	}
	_ = rows.Close()
	if tableExists == 0 {
		return nil, nil
	}

	// Try node_id suffix match
	escaped := escapeLikeMeta(symbol)
	rows, err = qg.QueryRefs(
		`SELECT node_id, ref_uri, ref_start_line, ref_start_col, ref_end_line, ref_end_col
		 FROM _lsp_refs WHERE node_id LIKE ? ESCAPE '\'`,
		"%/"+escaped,
	)
	if err != nil {
		return nil, fmt.Errorf("query _lsp_refs: %w", err)
	}

	var results []lspRefLocation
	for rows.Next() {
		var r lspRefLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	_ = rows.Close()

	// Fallback: broader LIKE match
	if len(results) == 0 {
		rows, err = qg.QueryRefs(
			`SELECT node_id, ref_uri, ref_start_line, ref_start_col, ref_end_line, ref_end_col
			 FROM _lsp_refs WHERE node_id LIKE ? ESCAPE '\'`,
			"%"+escaped+"%",
		)
		if err != nil {
			return nil, fmt.Errorf("query _lsp_refs (broad): %w", err)
		}
		for rows.Next() {
			var r lspRefLocation
			if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
				continue
			}
			results = append(results, r)
		}
		_ = rows.Close()
	}

	return results, nil
}

// escapeLikeMeta escapes SQL LIKE metacharacters (%, _, \) only.
// Safe with parameterized queries — does not modify quotes.
func escapeLikeMeta(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// escapeLikePattern escapes LIKE metacharacters AND doubles single quotes.
// For non-parameterized queries (daemon socket path) only.
func escapeLikePattern(s string) string {
	return strings.ReplaceAll(escapeLikeMeta(s), `'`, `''`)
}
