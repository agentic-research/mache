package testfixtures

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fixtureRootToken replaces the fixture's absolute source path everywhere it
// appears in a projection dump, so the golden is the same bytes on every
// machine and checkout.
const fixtureRootToken = "$FIXTURE"

// projectionSection is one table of the projection, dumped as sorted TSV rows.
// Columns that vary per build without the projection differing — mtime,
// mod_time, indexed_at — are left out, so two builds of the same source on
// different machines dump identically.
type projectionSection struct {
	name  string
	query string
	// render turns one scanned row into a TSV line; rows arrive as []any of
	// the column values in query order.
	render func(cols []any) string
}

// projectionSections is the byte-identity surface: every table the
// SQLiteWriter populates from a source tree. If the writer grows a table,
// TestGoldenProjection_CoversEveryWriterTable fails until it is listed here
// (or deliberately excluded there).
var projectionSections = []projectionSection{
	{
		name: "nodes",
		query: `SELECT id, COALESCE(parent_id, ''), name, kind, size, COALESCE(record_id, ''),
		               record, COALESCE(source_file, ''), context, COALESCE(props, '')
		        FROM nodes ORDER BY id`,
		render: func(c []any) string {
			return joinTSV(
				asString(c[0]), asString(c[1]), asString(c[2]), asString(c[3]), asString(c[4]),
				asString(c[5]), quoteBytes(c[6]), asString(c[7]), quoteBytes(c[8]), asString(c[9]),
			)
		},
	},
	{
		name:   "node_refs",
		query:  `SELECT token, node_id FROM node_refs ORDER BY token, node_id`,
		render: func(c []any) string { return joinTSV(asString(c[0]), asString(c[1])) },
	},
	{
		name:   "node_defs",
		query:  `SELECT token, node_id FROM node_defs ORDER BY token, node_id`,
		render: func(c []any) string { return joinTSV(asString(c[0]), asString(c[1])) },
	},
	{
		name:   "file_index",
		query:  `SELECT path, size FROM file_index ORDER BY path`,
		render: func(c []any) string { return joinTSV(asString(c[0]), asString(c[1])) },
	},
	{
		name: "_index_coverage",
		query: `SELECT source_id, producer, fidelity, complete FROM _index_coverage
		        ORDER BY source_id, producer`,
		render: func(c []any) string {
			return joinTSV(asString(c[0]), asString(c[1]), asString(c[2]), asString(c[3]))
		},
	},
}

// DumpProjection renders the projection db as one deterministic text: a
// `## <table>` header per section followed by its sorted, tab-separated rows,
// with fixtureRoot (and its symlink-resolved form) rewritten to $FIXTURE.
// Two builds of the same source with the same engine produce the same bytes;
// a differing byte is a projection change.
func DumpProjection(db *sql.DB, fixtureRoot string) (string, error) {
	roots := []string{fixtureRoot}
	if resolved, err := filepath.EvalSymlinks(fixtureRoot); err == nil && resolved != fixtureRoot {
		roots = append(roots, resolved)
	}
	// Longest first so a resolved prefix that contains the unresolved one is
	// rewritten whole, never left as "$FIXTURE" glued to a stray suffix.
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	pairs := make([]string, 0, 2*len(roots))
	for _, r := range roots {
		pairs = append(pairs, r, fixtureRootToken)
	}
	normalize := strings.NewReplacer(pairs...)

	var b strings.Builder
	for _, sec := range projectionSections {
		lines, err := dumpSection(db, sec)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "## %s (%d rows)\n", sec.name, len(lines))
		for _, l := range lines {
			b.WriteString(normalize.Replace(l))
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func dumpSection(db *sql.DB, sec projectionSection) ([]string, error) {
	rows, err := db.Query(sec.query)
	if err != nil {
		return nil, fmt.Errorf("dump %s: %w", sec.name, err)
	}
	defer func() { _ = rows.Close() }()
	colNames, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("dump %s: %w", sec.name, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("dump %s: %w", sec.name, err)
		}
		out = append(out, sec.render(vals))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dump %s: %w", sec.name, err)
	}
	// The queries ORDER BY already, but sort the rendered lines too so the
	// dump's order never depends on SQLite collation details.
	sort.Strings(out)
	return out, nil
}

func joinTSV(fields ...string) string { return strings.Join(fields, "\t") }

// asString renders a scanned scalar. NULL renders as "" — the queries COALESCE
// the nullable text columns, so a bare NULL here is a scan of a column the
// section did not expect to be nullable and is visible in the dump as such.
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}

// quoteBytes renders content columns (record, context) as a Go-quoted string
// on one line, so a golden diff shows the construct text that changed rather
// than an opaque hash. NULL renders as the bare word NULL, distinct from "".
func quoteBytes(v any) string {
	if v == nil {
		return "NULL"
	}
	return strconv.Quote(asString(v))
}

// diffLines is a set diff of the newline-separated rows of two dumps: rows
// only in want (removed by the change) and rows only in got (added). Section
// headers carry their row counts, so a count change shows up as a header pair.
func diffLines(want, got string) (removed, added []string) {
	wantSet := lineSet(want)
	gotSet := lineSet(got)
	for l := range wantSet {
		if _, ok := gotSet[l]; !ok {
			removed = append(removed, l)
		}
	}
	for l := range gotSet {
		if _, ok := wantSet[l]; !ok {
			added = append(added, l)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return removed, added
}

func lineSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			out[l] = struct{}{}
		}
	}
	return out
}
