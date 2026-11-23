package leyline

import (
	"errors"
	"fmt"

	daemonwire "github.com/agentic-research/ley-line-open/clients/go/leyline-schema/daemon/wire"
)

// QueryResult is one `query` reply: its rows in column order, and whether
// the daemon cut it short.
type QueryResult struct {
	Columns []string
	// Rows holds one value per column: a string for any non-NULL value (the
	// daemon sends integers and reals in decimal and blobs in hex), nil for
	// SQL NULL. NULL and the empty string are different values.
	Rows [][]any
	// TruncatedAt is the row limit the daemon applied when the statement had
	// more rows than it returned, and nil when Rows is the whole answer.
	TruncatedAt *uint32
}

// QueryTruncatedError is Query's refusal to pass a cut-short reply off as
// the answer. Limit is how many rows the daemon returned.
type QueryTruncatedError struct{ Limit uint32 }

func (e *QueryTruncatedError) Error() string {
	return fmt.Sprintf("query: the daemon returned only the first %d rows; the result is incomplete", e.Limit)
}

// QueryResult runs one read-only SELECT against the active arena through
// the daemon's `query` op.
//
// The reply is decoded by ley-line's own typed client (daemon/wire), which
// is the definition of the shape: each row is a list of cells in column
// order, a cell being exactly {"text": ...} or {"null": null}, and a cell
// naming any other member is refused. mache keeps no decoder of its own.
// Before wire major 3 a row was an object keyed by column name, and mache
// read it with a loose map: on a v3 daemon that reader returned the cell
// objects as if they were the values, with no error (mache-1f85ec).
func (c *SocketClient) QueryResult(sql string) (QueryResult, error) {
	var resp daemonwire.QueryResponse
	if err := c.SendOpInto(map[string]any{"op": "query", "sql": sql}, &resp); err != nil {
		var derr *DaemonResponseError
		if errors.As(err, &derr) {
			return QueryResult{}, fmt.Errorf("query: %s", derr.Message)
		}
		return QueryResult{}, err
	}
	if resp.Refusal != nil && resp.Refusal.Reason != nil {
		return QueryResult{}, fmt.Errorf("query refused: %s", *resp.Refusal.Reason)
	}
	out := QueryResult{Columns: resp.Columns, Rows: make([][]any, 0, len(resp.Rows))}
	for i, cells := range resp.Rows {
		if len(cells) != len(resp.Columns) {
			return QueryResult{}, fmt.Errorf("query: row %d has %d cells for %d columns", i, len(cells), len(resp.Columns))
		}
		row := make([]any, len(cells))
		for j, cell := range cells {
			if !cell.IsNull() {
				row[j] = *cell.Text
			}
		}
		out.Rows = append(out.Rows, row)
	}
	if resp.Truncation != nil {
		if resp.Truncation.Limit == nil {
			return QueryResult{}, errors.New("query: reply is truncated but names no limit")
		}
		out.TruncatedAt = resp.Truncation.Limit
	}
	return out, nil
}

// Query is QueryResult for a caller that needs the whole answer or none: a
// reply the daemon cut short is a *QueryTruncatedError, never a result.
func (c *SocketClient) Query(sql string) ([][]any, error) {
	res, err := c.QueryResult(sql)
	if err != nil {
		return nil, err
	}
	if res.TruncatedAt != nil {
		return nil, &QueryTruncatedError{Limit: *res.TruncatedAt}
	}
	return res.Rows, nil
}
