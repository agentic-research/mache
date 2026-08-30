package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lsp"
	"github.com/agentic-research/mache/internal/smells"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// lspEnrichHint is the user-facing guidance shown when LSP data is missing
// and ley-line daemon is not available. Single definition, used by both
// get_type_info and get_diagnostics handlers.
const lspEnrichHint = "Pre-enrich your database with ll-open, or install ley-line for live enrichment.\n" +
	"See: github.com/agentic-research/ley-line-open\n" +
	"Capability matrix: docs/ARCHITECTURE.md#interplay-with-ley-line-open"

// lspEnrichTimeout bounds how long mache waits for the daemon's synchronous
// `enrich` op (which spawns the language server and indexes the file). It
// MUST exceed the daemon's per-language per-file budget — gopls's is 50s
// (rust-analyzer indexes far faster). The prior 30s tripped mache's own
// deadline before the daemon's, surfacing a confusing client-side
// "i/o timeout" instead of the daemon's clean skip/result (mache-303036).
const lspEnrichTimeout = 90 * time.Second

func lspTableMissing(table, feature string) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf("no %s table in database. To get %s, either:\n"+
		"  1. Pre-enrich with: ll-open enrich-lsp (see github.com/agentic-research/ley-line-open)\n"+
		"  2. Pass a 'file' param to trigger live enrichment (requires ley-line daemon)\n"+
		"See docs/ARCHITECTURE.md#interplay-with-ley-line-open for which tools need which tables.", table, feature))
}

func lspEnrichFailed(feature string) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf("%s not available — LSP enrichment requires ley-line daemon.\n%s", feature, lspEnrichHint))
}

// relForEnrich makes filePath relative to the daemon's --source dir, which
// is what the `enrich` op expects ("files relative to source dir"). An
// absolute path under the source root is relativized; an already-relative
// path, or one outside the root, is passed through unchanged.
func relForEnrich(filePath string) string {
	src := leyline.DaemonSource()
	if src == "" || !filepath.IsAbs(filePath) {
		return filePath
	}
	rel, err := filepath.Rel(src, filePath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filePath
	}
	return rel
}

// formatNoHoverMessage builds the user-facing string returned when LSP
// enrichment completes but `_lsp_hover` has no row for the requested
// symbol. Per-pass skip reasons (from leyline-v0.5.3+) are appended so
// the operator sees WHY the enrich pass didn't write hover rows
// (rust-analyzer not on PATH, scope mismatched _source.id, language has
// no bundled server, etc.) instead of guessing.
func formatNoHoverMessage(symbol, kind string, skipReasons []string) string {
	var base string
	if kind != "" {
		base = fmt.Sprintf("LSP enrichment completed but no hover info found for %q with kind=%s", symbol, kind)
	} else {
		base = fmt.Sprintf("LSP enrichment completed but no hover info found for %q", symbol)
	}
	if len(skipReasons) == 0 {
		return base
	}
	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\nEnrichment pass skip reasons:")
	for _, r := range skipReasons {
		sb.WriteString("\n  - ")
		sb.WriteString(r)
	}
	return sb.String()
}

// extractEnrichSkipReasons returns the per-pass skip reasons surfaced by
// the daemon's `enrich` op response. Each pass's `skipped` field is a
// `[]string` carrying human-readable reasons that portions of the
// requested scope did not write rows (no bundled LSP server for the
// language, server not on PATH, scope matched no _source.id rows, etc.
// — see ley-line-open's EnrichmentStats.skipped, bead 661727).
//
// Pre-leyline-v0.5.3 daemons drop this field at the capnp wire boundary
// even when the JSON response carried it (Rust serde path worked, capnp
// path did not). Returns nil for those daemons — callers must accept
// nil-or-empty as "no skip-reason visibility available" rather than
// "definitely nothing was skipped." The leyline binary pin lives at
// `internal/leyline.leylineBinaryVersion`.
func extractEnrichSkipReasons(resp map[string]any) []string {
	passes, ok := resp["passes"].([]any)
	if !ok {
		return nil
	}
	var reasons []string
	for _, p := range passes {
		pmap, ok := p.(map[string]any)
		if !ok {
			continue
		}
		passName, _ := pmap["pass_name"].(string)
		skipped, ok := pmap["skipped"].([]any)
		if !ok {
			continue
		}
		for _, s := range skipped {
			str, ok := s.(string)
			if !ok || str == "" {
				continue
			}
			if passName != "" {
				reasons = append(reasons, fmt.Sprintf("[%s] %s", passName, str))
			} else {
				reasons = append(reasons, str)
			}
		}
	}
	return reasons
}

func makeGetTypeInfoHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		if symbol == "" {
			return mcp.NewToolResultError("symbol is required"), nil
		}
		kind, errResult := validateKindParam(request)
		if errResult != nil {
			return errResult, nil
		}

		// Fail fast on non-SQL backends with a clear message instead
		// of the generic post-assertion "LSP data not available" which
		// is the same string we use for "table missing." Today
		// lazyGraph always satisfies graph.RefsQuerier; this guards a future
		// backend that drops the method.
		qg, ok := g.(graph.RefsQuerier)
		if !ok {
			return mcp.NewToolResultError("get_type_info requires a SQL-capable graph backend (mache standalone or leyline-parsed .db)"), nil
		}

		// Check if _lsp_hover table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp_hover'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available for this data source"), nil
		}
		var refsTableExists int
		if rows.Next() {
			_ = rows.Scan(&refsTableExists)
		}
		_ = rows.Close()
		if refsTableExists == 0 {
			filePath := request.GetString("file", "")
			if filePath == "" {
				return lspTableMissing("_lsp_hover", "type info"), nil
			}
			result, err := enrichAndQueryTypeInfo(filePath, symbol, kind)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return lspEnrichFailed("type info"), nil
			}
			return result, nil
		}

		// LSP tables exist in mache's graph — query directly
		return queryTypeInfoFromGraph(qg, symbol, kind)
	}
}

func makeGetDiagnosticsHandler(g graph.Graph) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		symbol := request.GetString("symbol", "")
		limit := request.GetInt("limit", 50)
		kind, errResult := validateKindParam(request)
		if errResult != nil {
			return errResult, nil
		}

		// Mirror get_type_info: fail fast with a specific error if
		// the backend doesn't support SQL queries.
		qg, ok := g.(graph.RefsQuerier)
		if !ok {
			return mcp.NewToolResultError("get_diagnostics requires a SQL-capable graph backend (mache standalone or leyline-parsed .db)"), nil
		}

		// Check if _lsp table exists
		rows, err := qg.QueryRefs(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_lsp'`)
		if err != nil {
			return mcp.NewToolResultError("LSP data not available for this data source"), nil
		}
		var refsTableExists int
		if rows.Next() {
			_ = rows.Scan(&refsTableExists)
		}
		_ = rows.Close()
		if refsTableExists == 0 {
			filePath := request.GetString("file", "")
			if filePath == "" {
				return lspTableMissing("_lsp", "diagnostics"), nil
			}
			result, err := enrichAndQueryDiagnostics(filePath, symbol, kind, limit)
			if err != nil {
				log.Printf("LSP auto-enrichment failed: %v", err)
				return lspEnrichFailed("diagnostics"), nil
			}
			return result, nil
		}

		return queryDiagnosticsFromGraph(qg, symbol, kind, limit)
	}
}

// enrichAndQueryTypeInfo triggers LSP enrichment and queries hover data
// directly from the ley-line daemon's arena via UDS, bypassing mache's
// in-memory graph (which doesn't have _lsp* tables).
// Uses a single connection for both enrichment and query.
func enrichAndQueryTypeInfo(filePath, symbol, kind string) (*mcp.CallToolResult, error) {
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
	if err := client.SetDeadline(time.Now().Add(lspEnrichTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Enrich("lsp", []string{relForEnrich(filePath)})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)
	skipReasons := extractEnrichSkipReasons(resp)
	if len(skipReasons) > 0 {
		// Surface concretely so the operator sees WHY enrich silently
		// returned 0 rows. Requires ley-line-open ≥ v0.5.3 (bead 661727).
		log.Printf("LSP enrichment SKIPPED reasons (file=%s):", filePath)
		for _, r := range skipReasons {
			log.Printf("  - %s", r)
		}
	}

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
		return mcp.NewToolResultText(formatNoHoverMessage(symbol, "", skipReasons)), nil
	}

	results = filterByNodeIDKind(results, kind, func(r hoverResult) string { return r.NodeID })
	if len(results) == 0 {
		return mcp.NewToolResultText(formatNoHoverMessage(symbol, kind, skipReasons)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// queryTypeInfoFromGraph queries _lsp_hover from mache's in-memory graph
// (used when LSP tables already exist in the graph, e.g. from leyline lsp CLI).
func queryTypeInfoFromGraph(qg graph.RefsQuerier, symbol, kind string) (*mcp.CallToolResult, error) {
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

	results = filterByNodeIDKind(results, kind, func(r hoverResult) string { return r.NodeID })
	if len(results) == 0 {
		if kind != "" {
			return mcp.NewToolResultText(fmt.Sprintf("no type info found for %q with kind=%s", symbol, kind)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("no type info found for %q", symbol)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// enrichAndQueryDiagnostics triggers LSP enrichment and queries diagnostics
// directly from the ley-line daemon's arena via UDS.
// Uses a single connection for both enrichment and query.
func enrichAndQueryDiagnostics(filePath, symbol, kind string, limit int) (*mcp.CallToolResult, error) {
	sockPath, err := leyline.DiscoverOrStart()
	if err != nil {
		return nil, fmt.Errorf("discover/start leyline: %w", err)
	}
	client, err := leyline.DialSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.SetDeadline(time.Now().Add(lspEnrichTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := client.Enrich("lsp", []string{relForEnrich(filePath)})
	if err != nil {
		return nil, err
	}
	log.Printf("LSP enrichment via ley-line daemon: %v", resp)
	skipReasons := extractEnrichSkipReasons(resp)
	if len(skipReasons) > 0 {
		log.Printf("LSP enrichment SKIPPED reasons (file=%s):", filePath)
		for _, r := range skipReasons {
			log.Printf("  - %s", r)
		}
	}

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

	results = filterByNodeIDKind(results, kind, func(r diagResult) string { return r.NodeID })
	if len(results) == 0 {
		return noDiagnosticsResultWithSkipReasons(symbol, kind, skipReasons), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// noDiagnosticsResult returns the appropriate "no diagnostics found"
// message based on which filter inputs were set. Extracted from the
// two diagnostics paths (enrich + in-graph) which would otherwise
// repeat the switch verbatim.
func noDiagnosticsResult(symbol, kind string) *mcp.CallToolResult {
	switch {
	case symbol != "" && kind != "":
		return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q with kind=%s", symbol, kind))
	case symbol != "":
		return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found for %q", symbol))
	case kind != "":
		return mcp.NewToolResultText(fmt.Sprintf("no diagnostics found with kind=%s", kind))
	default:
		return mcp.NewToolResultText("no diagnostics found")
	}
}

// noDiagnosticsResultWithSkipReasons appends per-pass enrich skip
// reasons to the base "no diagnostics found" message so the operator
// sees WHY the daemon didn't write any diagnostic rows for the file.
// Requires ley-line-open ≥ v0.5.3 to populate skipReasons; older
// daemons return an empty slice (silent path).
func noDiagnosticsResultWithSkipReasons(symbol, kind string, skipReasons []string) *mcp.CallToolResult {
	base := noDiagnosticsResult(symbol, kind)
	if len(skipReasons) == 0 {
		return base
	}
	var sb strings.Builder
	for _, c := range base.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	sb.WriteString("\n\nEnrichment pass skip reasons:")
	for _, r := range skipReasons {
		sb.WriteString("\n  - ")
		sb.WriteString(r)
	}
	return mcp.NewToolResultText(sb.String())
}

// queryDiagnosticsFromGraph queries _lsp from mache's in-memory graph.
func queryDiagnosticsFromGraph(qg graph.RefsQuerier, symbol, kind string, limit int) (*mcp.CallToolResult, error) {
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

	results = filterByNodeIDKind(results, kind, func(r diagResult) string { return r.NodeID })
	if len(results) == 0 {
		return noDiagnosticsResult(symbol, kind), nil
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

// queryLSPDefs queries the _lsp_defs table for definition locations
// of a symbol. Returns (nil, nil) if the table does not exist.
//
// Per ADR-0013 Step 5 (mache-346d2b), this prefers the direct
// def_token match when ley-line-open's post-Step-1 column is
// present. The legacy suffix-then-broader-LIKE fallback survives
// for pre-Step-1 .dbs that still have only the (node_id, def_uri,
// ...) columns.
func queryLSPDefs(qg graph.RefsQuerier, symbol string) ([]lspDefLocation, error) {
	hasDefToken, err := smells.TableHasColumn(qg, "_lsp_defs", "def_token")
	if err != nil {
		return nil, fmt.Errorf("probe _lsp_defs schema: %w", err)
	}
	if !hasDefToken {
		// Pre-Step-1 schema (or table missing entirely). Fall back to
		// node_id LIKE matching against the trailing component, with
		// a broader substring fallback if that returns nothing.
		return queryLSPDefsLegacy(qg, symbol)
	}

	rows, err := qg.QueryRefs(
		`SELECT node_id, def_uri, def_start_line, def_start_col, def_end_line, def_end_col
		 FROM _lsp_defs
		 WHERE def_token = ?`,
		symbol,
	)
	if err != nil {
		return nil, fmt.Errorf("query _lsp_defs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []lspDefLocation
	for rows.Next() {
		var r lspDefLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// queryLSPRefs returns reference locations for a symbol.
//
// Per mache-6bd4d8 (T8.8 mirror): the primary source is the sibling
// `.bindings.capnp` event log, read via internal/lsp.ReadBindingLog.
// `_lsp_refs` SQL columns were retired as the consumer-side contract
// — capnp records carry the full (refToken, refUri, refRange) tuple
// without a SQL JOIN, structurally precluding be6136-class column-
// name-as-protocol disagreements.
//
// Falls back to the legacy SQL path (queryLSPRefsLegacy) only when
// the sibling capnp log is absent AND the .db has the pre-Step-1
// `_lsp_refs` schema (no ref_token column). That window is narrow:
// post-LLO-T8.2 builds always emit the capnp log.
//
// Returns (nil, nil) when neither source has data for the symbol.
func queryLSPRefs(qg graph.RefsQuerier, symbol string) ([]lspRefLocation, error) {
	// Capnp source: requires the querier to know its dbPath
	// (graph.DBPathProvider opt-in, mache-190508 step 3). When available,
	// it's the canonical source.
	if dp, ok := qg.(graph.DBPathProvider); ok {
		if results, err := readLSPRefsFromCapnp(dp.DBPath(), symbol); err != nil {
			return nil, err
		} else if results != nil {
			return results, nil
		}
		// Capnp present but no match for this symbol → empty slice
		// vs nil distinction handled by readLSPRefsFromCapnp:
		// nil means "no log to consult", []lspRefLocation{} means
		// "log present, zero matches". Fall through only on nil.
	}

	// Legacy path: only fires when capnp is absent. queryLSPRefsLegacy
	// handles both the post-Step-1 schema (with ref_token column) and
	// the pre-Step-1 LIKE-suffix shape.
	return queryLSPRefsLegacy(qg, symbol)
}

// readLSPRefsFromCapnp reads the sibling .bindings.capnp event log and
// filters records by RefToken == symbol. Returns:
//
//	(nil, nil)      — no sibling log exists; caller should fall back
//	([]..., nil)    — log exists; slice contains all matches (possibly empty)
//	(nil, err)      — log exists but couldn't be read
//
// dbPath empty string is treated as "no source" (nil, nil).
func readLSPRefsFromCapnp(dbPath, symbol string) ([]lspRefLocation, error) {
	if dbPath == "" {
		return nil, nil
	}
	logPath := lsp.SiblingBindingLogPath(dbPath)
	records, err := lsp.ReadBindingLog(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read binding log %s: %w", logPath, err)
	}

	// Intentional empty (not nil) slice: "log present, zero matches"
	// is meaningfully different from "no log to consult" — see the
	// caller's nil-check.
	results := []lspRefLocation{}
	for _, r := range records {
		if r.RefToken != symbol {
			continue
		}
		results = append(results, lspRefLocation{
			NodeID:    r.TargetNodeID,
			URI:       r.RefURI,
			StartLine: int(r.RefRange.StartLine),
			StartCol:  int(r.RefRange.StartColumn),
			EndLine:   int(r.RefRange.EndLine),
			EndCol:    int(r.RefRange.EndColumn),
		})
	}
	return results, nil
}

// lspLegacyTable names one of the two legacy LSP tables and its column
// prefix. Unexported with only two package-level instances, so the legacy
// query builder cannot be handed a name from anywhere else — the identifier
// splice is safe by construction rather than by a runtime check that no
// reachable call site could ever trip.
type lspLegacyTable struct{ table, colPrefix string }

var (
	lspLegacyDefs = lspLegacyTable{table: "_lsp_defs", colPrefix: "def"}
	lspLegacyRefs = lspLegacyTable{table: "_lsp_refs", colPrefix: "ref"}
)

// lspLegacyLocation is the row shape both legacy tables produce. An ALIAS,
// not a third struct: _lsp_defs and _lsp_refs are field-for-field identical,
// and the shared query below should not invent a type to convert through.
type lspLegacyLocation = lspDefLocation

// queryLSPLocationsLegacy is the pre-Step-1 query path shared by both legacy
// tables: node_id suffix match, then a broader substring LIKE if that returns
// nothing. Kept for .dbs that still carry the legacy schema (no *_token
// column); new consumers rely on queryLSPDefs/queryLSPRefs, which prefer the
// direct token match when it is available.
//
// This exists because the defs and refs versions were the largest production
// clone in the repo — a 284-signature pair differing only in table name,
// column prefix, and row type, with the code itself saying so
// ("mirrors queryLSPDefsLegacy for the refs table"). internal/leylinegraph
// had already collapsed the same defs/refs mirroring behind loadLSPLocations;
// this applies the answer the repo had already chosen (mache-4a92cf).
//
// The table and column prefix are interpolated, not bound — SQLite takes no
// parameters for identifiers. Rather than validating a string at runtime, the
// only two legal targets are values of an unexported type, so an arbitrary
// name cannot reach the query builder at all.
func queryLSPLocationsLegacy(qg graph.RefsQuerier, t lspLegacyTable, symbol string) ([]lspLegacyLocation, error) {
	table, colPrefix := t.table, t.colPrefix
	// tableHasColumn returns false for both "no table" and "table without
	// column", so the legacy path has to disambiguate here.
	exists, err := refsTableExists(qg, table)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	query := fmt.Sprintf(
		`SELECT node_id, %[1]s_uri, %[1]s_start_line, %[1]s_start_col, %[1]s_end_line, %[1]s_end_col
		 FROM %[2]s WHERE node_id LIKE ? ESCAPE '\'`, colPrefix, table)

	escaped := escapeLikeMeta(symbol)
	results, err := scanLSPLegacyRows(qg, query, "%/"+escaped)
	if err != nil {
		return nil, fmt.Errorf("query %s (legacy suffix): %w", table, err)
	}
	if len(results) > 0 {
		return results, nil
	}

	results, err = scanLSPLegacyRows(qg, query, "%"+escaped+"%")
	if err != nil {
		return nil, fmt.Errorf("query %s (legacy broad): %w", table, err)
	}
	return results, nil
}

// scanLSPLegacyRows runs one legacy lookup. A row that fails to scan is
// skipped rather than failing the query, preserving the original behaviour:
// a malformed row in a legacy table must not hide the rows around it.
func scanLSPLegacyRows(qg graph.RefsQuerier, query, pattern string) ([]lspLegacyLocation, error) {
	rows, err := qg.QueryRefs(query, pattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []lspLegacyLocation
	for rows.Next() {
		var r lspLegacyLocation
		if err := rows.Scan(&r.NodeID, &r.URI, &r.StartLine, &r.StartCol, &r.EndLine, &r.EndCol); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// queryLSPDefsLegacy is the defs half of the legacy path.
func queryLSPDefsLegacy(qg graph.RefsQuerier, symbol string) ([]lspDefLocation, error) {
	return queryLSPLocationsLegacy(qg, lspLegacyDefs, symbol)
}

// queryLSPRefsLegacy is the refs half. The conversion is free — the two row
// types are structurally identical — and keeping them DISTINCT is deliberate:
// a definition and a reference are different facts, and the compiler should
// keep refusing to pass one where the other belongs.
func queryLSPRefsLegacy(qg graph.RefsQuerier, symbol string) ([]lspRefLocation, error) {
	rows, err := queryLSPLocationsLegacy(qg, lspLegacyRefs, symbol)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// Preserve the (nil, nil) contract: callers read a nil slice as "no
		// legacy table / nothing to say". An empty non-nil slice would be a
		// different claim, and a caught regression — allocating the result
		// unconditionally broke TestQueryLSPDefs_TableMissing_StillReturnsNilNil.
		return nil, nil
	}
	out := make([]lspRefLocation, 0, len(rows))
	for _, r := range rows {
		out = append(out, lspRefLocation(r))
	}
	return out, nil
}

// refsTableExists returns true iff the named SQLite table is present in
// the active database. Used by queryLSP*Legacy to short-circuit when
// the table doesn't exist (consistent with the original "(nil, nil)
// = no table" contract).
func refsTableExists(qg graph.RefsQuerier, name string) (bool, error) {
	if !smells.IsSimpleIdent(name) {
		return false, fmt.Errorf("invalid table name: %q", name)
	}
	rows, err := qg.QueryRefs(
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name = ?`,
		name,
	)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	return rows.Next(), rows.Err()
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
