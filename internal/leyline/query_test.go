package leyline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryAgainst dials a fake daemon whose `query` op answers reply.
func queryAgainst(t *testing.T, reply map[string]any) *SocketClient {
	t.Helper()
	sock := mockServer(t, func(map[string]any) map[string]any { return reply })
	client, err := DialSocket(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func text(s string) map[string]any { return map[string]any{"text": s} }

var null = map[string]any{"null": nil}

// Wire major 3: a row is a list of cells in column order. Every non-NULL
// value arrives as text; NULL is its own member, so it is not the empty
// string; and two columns with one name both keep their value, which a row
// keyed by column name could not do (mache-1f85ec).
func TestQuery_ReadsCellRowsInColumnOrder(t *testing.T) {
	client := queryAgainst(t, map[string]any{
		"ok":      true,
		"columns": []any{"node_id", "n", "note", "note"},
		"rows": []any{
			[]any{text("symbols/config"), text("42"), text(""), null},
			[]any{text("symbols/auth"), text("2.5"), text("first"), text("second")},
		},
	})
	res, err := client.QueryResult("SELECT 1")
	require.NoError(t, err)
	assert.Equal(t, []string{"node_id", "n", "note", "note"}, res.Columns)
	assert.Equal(t, [][]any{
		{"symbols/config", "42", "", nil},
		{"symbols/auth", "2.5", "first", "second"},
	}, res.Rows)
	assert.Nil(t, res.TruncatedAt, "a reply without truncation is the whole answer")

	rows, err := client.Query("SELECT 1")
	require.NoError(t, err)
	assert.Equal(t, res.Rows, rows)
}

// A cell is exactly {"text": ...} or {"null": null}. Anything else is a
// shape this build does not know, and is refused rather than guessed at:
// including the pre-major-3 row (an object keyed by column) and a bare
// value, which the old loose reader passed through as data.
func TestQuery_RefusesShapesItDoesNotKnow(t *testing.T) {
	for name, rows := range map[string][]any{
		"a cell member this build does not know": {[]any{map[string]any{"integer": 7}}},
		"a cell with two members":                {[]any{map[string]any{"text": "a", "null": nil}}},
		"an empty cell":                          {[]any{map[string]any{}}},
		"a bare value where a cell belongs":      {[]any{"node_id_1"}},
		"a row keyed by column (wire major 2)":   {map[string]any{"c": "v"}},
		"rows that are not a list":               nil,
	} {
		t.Run(name, func(t *testing.T) {
			reply := map[string]any{"ok": true, "columns": []any{"c"}, "rows": any(rows)}
			if rows == nil {
				reply["rows"] = "not-an-array"
			}
			_, err := queryAgainst(t, reply).QueryResult("SELECT 1")
			require.Error(t, err)
		})
	}
}

// A row with more or fewer cells than there are columns cannot be read by
// position.
func TestQuery_RefusesARowThatDoesNotMatchItsColumns(t *testing.T) {
	_, err := queryAgainst(t, map[string]any{
		"ok": true, "columns": []any{"a", "b"}, "rows": []any{[]any{text("only one")}},
	}).QueryResult("SELECT 1")
	require.ErrorContains(t, err, "1 cells for 2 columns")
}

// A reply the daemon cut short says so, and is never the whole answer:
// QueryResult returns the rows it got with the limit, and Query, for callers
// that need all of it, returns an error and no rows.
func TestQuery_ATruncatedReplyIsNotACompleteOne(t *testing.T) {
	reply := map[string]any{
		"ok": true, "columns": []any{"c"},
		"rows":       []any{[]any{text("a")}, []any{text("b")}},
		"truncation": map[string]any{"limit": 2},
	}
	res, err := queryAgainst(t, reply).QueryResult("SELECT 1")
	require.NoError(t, err)
	require.NotNil(t, res.TruncatedAt)
	assert.Equal(t, uint32(2), *res.TruncatedAt)
	assert.Len(t, res.Rows, 2, "the partial rows are still returned, marked")

	rows, err := queryAgainst(t, reply).Query("SELECT 1")
	var truncated *QueryTruncatedError
	require.ErrorAs(t, err, &truncated)
	assert.Equal(t, uint32(2), truncated.Limit)
	assert.Nil(t, rows)

	// The removed top-level keys of earlier wires mean nothing now.
	old := map[string]any{"ok": true, "columns": []any{"c"}, "rows": []any{[]any{text("a")}}, "truncated": true, "limit": 1}
	res, err = queryAgainst(t, old).QueryResult("SELECT 1")
	require.NoError(t, err)
	assert.Nil(t, res.TruncatedAt, "`truncated` and `limit` at the top level are not read")
}

// A statement the daemon refuses, or fails to run, is an error naming why.
func TestQuery_SurfacesRefusalsAndErrors(t *testing.T) {
	_, err := queryAgainst(t, map[string]any{
		"ok": false, "refusal": map[string]any{"reason": "limit_out_of_range"},
	}).Query("SELECT 1")
	require.ErrorContains(t, err, "limit_out_of_range")

	_, err = queryAgainst(t, map[string]any{"ok": false, "error": "no such table: t"}).Query("SELECT 1")
	require.ErrorContains(t, err, "no such table: t")
}
