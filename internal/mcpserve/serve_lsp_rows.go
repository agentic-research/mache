package mcpserve

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
)

// The LSP tools read ley-line-open's _lsp* tables from one of two places:
// mache's own graph, when the served db was enriched before it was opened, or
// the leyline daemon's arena, right after a live `enrich`. Both are LLO's
// writing, so both change shape with the projection: v4 keys every row by
// node_id, a path, and v6 by an integer nid that only renders as one
// (mache-b1f7b8). Everything below reads either shape from either place, once.

// lspSource runs one statement where the _lsp* tables are and returns its rows
// as values. It takes no parameters, because the daemon's query op takes none.
type lspSource func(query string) ([][]any, error)

// graphLSPSource reads from mache's own graph.
func graphLSPSource(qg graph.RefsQuerier) lspSource {
	return func(query string) ([][]any, error) {
		rows, err := qg.QueryRefs(query)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		var out [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			out = append(out, vals)
		}
		return out, rows.Err()
	}
}

// lspTable is one of the LSP tables this file reads. Its values are the
// constants below, so a name spliced into a statement is never caller text.
type lspTable string

const (
	lspSymbols lspTable = "_lsp"
	lspHover   lspTable = "_lsp_hover"
	lspDefs    lspTable = "_lsp_defs"
)

// lspRow is one row of an LSP table: the node it is about, as a path, and the
// columns asked for, in order.
type lspRow struct {
	path string
	cols []any
}

// readLSPRows selects cols from table, with each row's node as a path, sorted
// by path so both shapes answer in the same order. A v6 row whose nid names no
// node is dropped, as SQLiteGraph's lookups drop one: it names nothing a
// consumer can open. cols and where are this package's constant SQL, plus
// literals written by sqlLiteral.
func readLSPRows(src lspSource, table lspTable, cols, where string) ([]lspRow, error) {
	v6, err := lspKeyedByNid(src, table)
	if err != nil {
		return nil, err
	}
	key := "node_id"
	var render *lloschema.PathRenderer
	if v6 {
		key = "nid"
		render = lloschema.ValuesRenderer(src)
	}
	q := "SELECT " + key + ", " + cols + " FROM " + string(table)
	if where != "" {
		q += " WHERE " + where
	}
	rows, err := src(q)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", table, err)
	}
	out := make([]lspRow, 0, len(rows))
	for _, r := range rows {
		if len(r) == 0 {
			continue
		}
		path := textValue(r[0])
		if v6 {
			nid, err := lloschema.IntValue(r[0])
			if err != nil {
				return nil, fmt.Errorf("%s.nid: %w", table, err)
			}
			p, ok, err := render.Path(nid)
			if err != nil {
				return nil, fmt.Errorf("render %s nid %d: %w", table, nid, err)
			}
			if !ok {
				continue
			}
			path = p
		}
		out = append(out, lspRow{path: path, cols: r[1:]})
	}
	slices.SortStableFunc(out, func(a, b lspRow) int { return cmp.Compare(a.path, b.path) })
	return out, nil
}

// lspKeyedByNid reports whether LLO keyed table by nid (projection-v6) rather
// than node_id. Probed, not read off a version: the db carries its own answer.
func lspKeyedByNid(src lspSource, table lspTable) (bool, error) {
	rows, err := src("SELECT COUNT(*) FROM pragma_table_info('" + string(table) + "') WHERE name = 'nid'")
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", table, err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return false, fmt.Errorf("probe %s: no answer", table)
	}
	n, err := lloschema.IntValue(rows[0][0])
	return n > 0, err
}

// pathsNaming keeps the rows whose path names symbol. With suffixFirst, a path
// ending in "/"+symbol wins, and only when none does is a path containing it
// enough. Matching folds ASCII case, as the SQL LIKE these lookups used to be
// written in did.
func pathsNaming(rows []lspRow, symbol string, suffixFirst bool) []lspRow {
	sym := likeFold(symbol)
	keep := func(match func(path string) bool) []lspRow {
		var out []lspRow
		for _, r := range rows {
			if match(likeFold(r.path)) {
				out = append(out, r)
			}
		}
		return out
	}
	if suffixFirst {
		if out := keep(func(p string) bool { return strings.HasSuffix(p, "/"+sym) }); len(out) > 0 {
			return out
		}
	}
	return keep(func(p string) bool { return strings.Contains(p, sym) })
}

// likeFold lowers ASCII letters only, which is the case folding SQLite's LIKE
// does.
func likeFold(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// sqlLiteral is s as a SQL string literal.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// textValue is a TEXT column's value, "" for NULL.
func textValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// intColumn is an INTEGER column's value.
func intColumn(v any) (int, error) {
	n, err := lloschema.IntValue(v)
	return int(n), err
}

type hoverResult struct {
	NodeID    string `json:"node_id"`
	HoverText string `json:"hover_text"`
}

// lookupHover is the hover text for symbol, matched against each symbol's
// standalone address (symbols/Store/Add), then narrowed to kind.
func lookupHover(src lspSource, symbol, kind string) ([]hoverResult, error) {
	rows, err := readLSPRows(src, lspHover, "hover_text", "")
	if err != nil {
		return nil, err
	}
	var out []hoverResult
	for _, r := range pathsNaming(rows, symbol, true) {
		out = append(out, hoverResult{NodeID: r.path, HoverText: textValue(r.cols[0])})
	}
	return filterByNodeIDKind(out, kind, func(r hoverResult) string { return r.NodeID }), nil
}

type diagResult struct {
	NodeID      string `json:"node_id"`
	SymbolKind  string `json:"symbol_kind"`
	Diagnostics string `json:"diagnostics"`
}

// lookupDiagnostics is up to limit symbols that carry diagnostics, those whose
// node's path contains symbol when one is given, narrowed to kind.
func lookupDiagnostics(src lspSource, symbol, kind string, limit int) ([]diagResult, error) {
	rows, err := readLSPRows(src, lspSymbols, "symbol_kind, diagnostics",
		"diagnostics IS NOT NULL AND diagnostics != ''")
	if err != nil {
		return nil, err
	}
	if symbol != "" {
		rows = pathsNaming(rows, symbol, false)
	}
	var out []diagResult
	for _, r := range rows {
		out = append(out, diagResult{NodeID: r.path, SymbolKind: textValue(r.cols[0]), Diagnostics: textValue(r.cols[1])})
	}
	out = filterByNodeIDKind(out, kind, func(r diagResult) string { return r.NodeID })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// lookupLSPDefs is every _lsp_defs row whose def_token is symbol.
func lookupLSPDefs(src lspSource, symbol string) ([]lspDefLocation, error) {
	rows, err := readLSPRows(src, lspDefs,
		"def_uri, def_start_line, def_start_col, def_end_line, def_end_col",
		"def_token = "+sqlLiteral(symbol))
	if err != nil {
		return nil, err
	}
	out := make([]lspDefLocation, 0, len(rows))
	for _, r := range rows {
		d := lspDefLocation{NodeID: r.path, URI: textValue(r.cols[0])}
		for i, dst := range []*int{&d.StartLine, &d.StartCol, &d.EndLine, &d.EndCol} {
			if *dst, err = intColumn(r.cols[i+1]); err != nil {
				return nil, fmt.Errorf("_lsp_defs: %w", err)
			}
		}
		out = append(out, d)
	}
	return out, nil
}
