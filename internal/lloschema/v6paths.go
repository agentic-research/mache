package lloschema

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/agentic-research/mache/internal/sqlstmt"
)

// V6Paths addresses projection-v6 nodes by path.
//
// projection-v6 stores no path strings. A node is an integer nid, and its path
// is what LLO's naming rule renders from the parent chain: directories and
// files by their interned names, syntax nodes as their raw kind plus "_k" when
// the kind repeats among siblings. mache's surfaces (NFS paths, MCP node ids)
// speak paths, so every read that starts from one has to turn it into a nid
// and back.
//
// LLO's v_node_path view answers both directions, and must never be used for
// either on a hot path. It is a recursive CTE that cannot take the caller's
// row as its anchor, so a lookup renders the path of EVERY node in the arena
// and filters afterwards: 12.6 s for one lookup at 632k nodes (mache-90cb9f).
// The cost grows with the database, not with the path.
//
// So both directions here are anchored at the node asked about, and cost a
// number of statements bounded by the path's depth:
//
//   - Path walks UP from the nid, naming each ancestor through LLO's own
//     v_node_name, which is a per-row view and anchors cleanly. The naming
//     rule stays LLO's; nothing here re-derives a name.
//   - Resolve walks DOWN segment by segment, mirroring ley-line-open's
//     resolve_path (ll-core/schema, v0.20.3). It has to restate the naming
//     rule in reverse, because LLO ships that inverse as Rust only, not as a
//     view. Where LLO is lenient it is lenient in the same way: a "_k" suffix
//     on a kind that does not repeat still resolves to the k-th sibling, as it
//     does in LLO, so a path resolves here exactly when it resolves there.
//
// Statements are compiled once and reused (sqlstmt.Lazy), as the
// nodes-table reader's are.
type V6Paths struct {
	db                                *sql.DB
	path, file, dir, single, nth, hop *sqlstmt.Lazy
}

// V6RootNid is the root directory's nid: directory ids are stored negated,
// and the root is directory 1.
const V6RootNid = -1

// V6FileShift places a file's id in the top bits of every nid below it: a
// file is file_id << V6FileShift, and the syntax nodes in it add their
// preorder ordinal. So nid >> V6FileShift is the file a node belongs to.
const V6FileShift = 24

// V6FileNids is a SQL condition on the nid column col that holds for exactly
// the nodes of one file, the one whose _source id is the statement's FIRST
// parameter. It is a range on the nid, so a primary-key search, where
// filtering on the file id derived from each row (nid >> V6FileShift) would
// scan the table. The file id is looked up once: the subquery is constant.
func V6FileNids(col string) string {
	file := "(SELECT file_id FROM _source WHERE id = ?1)"
	shift := strconv.Itoa(V6FileShift)
	return col + " >= " + file + " << " + shift + " AND " + col + " < (" + file + " + 1) << " + shift
}

// NewV6Paths returns path addressing over a projection-v6 database. The
// caller owns db; call Close before closing it.
func NewV6Paths(db *sql.DB) *V6Paths {
	return &V6Paths{
		db: db,
		path: sqlstmt.NewLazy(`WITH RECURSIVE up(nid, parent, depth) AS (
  SELECT nid, parent_nid, 0 FROM nodes WHERE nid = ?
  UNION ALL
  SELECT n.nid, n.parent_nid, up.depth + 1 FROM nodes n JOIN up ON n.nid = up.parent
)
SELECT v.name FROM up JOIN v_node_name v ON v.nid = up.nid ORDER BY up.depth DESC`),
		file: sqlstmt.NewLazy(`SELECT f.file_id FROM files f JOIN names n ON n.name_id = f.name_id
WHERE f.dir_id = ? AND n.text = ?`),
		dir: sqlstmt.NewLazy(`SELECT d.dir_id FROM dirs d JOIN names n ON n.name_id = d.name_id
WHERE d.parent_dir_id = ? AND n.text = ?`),
		// A bare kind names a node only when exactly one sibling has it: the
		// writer suffixes every one of them otherwise.
		single: sqlstmt.NewLazy(`SELECT CASE WHEN COUNT(*) = 1 THEN MIN(n.nid) END
FROM nodes n JOIN kinds k ON k.kind_id = n.kind_id
WHERE n.parent_nid = ? AND k.raw_kind = ?`),
		nth: sqlstmt.NewLazy(`SELECT n.nid FROM nodes n JOIN kinds k ON k.kind_id = n.kind_id
WHERE n.parent_nid = ? AND k.raw_kind = ?
ORDER BY n.ord LIMIT 1 OFFSET ?`),
		hop: sqlstmt.NewLazy(hopSQL),
	}
}

// Close releases the compiled statements. It does not close the db.
func (p *V6Paths) Close() {
	for _, s := range []*sqlstmt.Lazy{p.path, p.file, p.dir, p.single, p.nth, p.hop} {
		s.Close()
	}
}

// Path returns the path nid renders as, and false when there is no such node.
// The root renders as "".
func (p *V6Paths) Path(nid int64) (string, bool, error) {
	stmt, err := p.path.Get(p.db)
	if err != nil {
		return "", false, err
	}
	rows, err := stmt.Query(nid)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	var segs []string
	found := false
	for rows.Next() {
		found = true
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", false, err
		}
		// Only the root has an empty name, and it contributes no segment.
		if name != "" {
			segs = append(segs, name)
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return strings.Join(segs, "/"), found, nil
}

// Resolve returns the nid path renders from, and false when no node does.
// "" is the root directory.
func (p *V6Paths) Resolve(path string) (int64, bool, error) {
	if path == "" {
		return V6RootNid, true, nil
	}
	segs := strings.Split(path, "/")
	// Directories and files first: descend directories while segments name
	// them, until one names a file, below which every segment is syntax.
	dirID := int64(1)
	i := 0
	var cur int64
	inFile := false
	for ; i < len(segs) && !inFile; i++ {
		fileID, ok, err := p.one(p.file, dirID, segs[i])
		if err != nil {
			return 0, false, err
		}
		if ok {
			cur, inFile = fileID<<V6FileShift, true
			continue
		}
		sub, ok, err := p.one(p.dir, dirID, segs[i])
		if err != nil || !ok {
			return 0, false, err
		}
		dirID = sub
	}
	if !inFile {
		return -dirID, true, nil
	}
	for _, seg := range segs[i:] {
		next, ok, err := p.syntaxChild(cur, seg)
		if err != nil || !ok {
			return 0, false, err
		}
		cur = next
	}
	return cur, true, nil
}

// syntaxChild resolves one syntax segment under parent: the bare form first,
// then a trailing "_k" as the k-th sibling of that kind by ord.
func (p *V6Paths) syntaxChild(parent int64, seg string) (int64, bool, error) {
	if nid, ok, err := p.one(p.single, parent, seg); err != nil || ok {
		return nid, ok, err
	}
	kind, rank, ok := splitRank(seg)
	if !ok {
		return 0, false, nil
	}
	return p.one(p.nth, parent, kind, rank)
}

// splitRank splits a "kind_k" segment at its LAST underscore, when what follows
// is all digits: raw kinds contain underscores themselves. A rank too large
// for an int64 names no sibling, rather than wrapping to one that exists.
func splitRank(seg string) (string, int64, bool) {
	i := strings.LastIndexByte(seg, '_')
	if i < 0 || i == len(seg)-1 {
		return "", 0, false
	}
	digits := seg[i+1:]
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return "", 0, false
		}
	}
	rank, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return seg[:i], rank, true
}

// one runs a single-value lookup. A NULL answer (the bare-kind query's "not
// exactly one") is not found, the same as no row.
func (p *V6Paths) one(s *sqlstmt.Lazy, args ...any) (int64, bool, error) {
	stmt, err := s.Get(p.db)
	if err != nil {
		return 0, false, err
	}
	var v sql.NullInt64
	if err := stmt.QueryRow(args...).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// PathRenderer renders the paths of many nodes, naming each ancestor once.
//
// Path walks a node's whole ancestry per call, which is right for one node
// and wasteful for a table's worth: the references in a file share every
// ancestor down to their functions. A renderer remembers each path it builds,
// so rendering n nodes costs one statement per DISTINCT node on their paths,
// however many rows name them. It is LLO's own PathRenderer
// (ll-core/schema), for the same reason. Use one per batch and drop it; it
// does not see later changes to the database.
type PathRenderer struct {
	// hop reads one node's parent and name; ok is false when there is no
	// such node.
	hop  func(nid int64) (parent sql.NullInt64, name string, ok bool, err error)
	memo map[int64]string
}

// hopSQL is one step up: a node's parent and its name, from LLO's own
// v_node_name.
const hopSQL = `SELECT n.parent_nid, v.name FROM nodes n JOIN v_node_name v ON v.nid = n.nid WHERE n.nid = ?`

// Renderer returns a renderer over p's database, on p's compiled statement.
func (p *V6Paths) Renderer() *PathRenderer {
	return newRenderer(func(nid int64) (*sql.Row, error) {
		stmt, err := p.hop.Get(p.db)
		if err != nil {
			return nil, err
		}
		return stmt.QueryRow(nid), nil
	})
}

// QuerierRenderer returns a renderer for a caller that holds a Querier rather
// than the database: the smell engine, which reads through whatever graph
// backend it was given.
func QuerierRenderer(qg Querier) *PathRenderer {
	return &PathRenderer{memo: map[int64]string{}, hop: func(nid int64) (sql.NullInt64, string, bool, error) {
		rows, err := qg.QueryRefs(hopSQL, nid)
		if err != nil {
			return sql.NullInt64{}, "", false, err
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return sql.NullInt64{}, "", false, rows.Err()
		}
		var parent sql.NullInt64
		var name string
		if err := rows.Scan(&parent, &name); err != nil {
			return sql.NullInt64{}, "", false, err
		}
		return parent, name, true, nil
	}}
}

func newRenderer(row func(nid int64) (*sql.Row, error)) *PathRenderer {
	return &PathRenderer{memo: map[int64]string{}, hop: func(nid int64) (sql.NullInt64, string, bool, error) {
		var parent sql.NullInt64
		var name string
		r, err := row(nid)
		if err != nil {
			return parent, name, false, err
		}
		if err := r.Scan(&parent, &name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return parent, name, false, nil
			}
			return parent, name, false, err
		}
		return parent, name, true, nil
	}}
}

// Path returns the path nid renders as, and false when there is no such node.
func (r *PathRenderer) Path(nid int64) (string, bool, error) {
	if path, ok := r.memo[nid]; ok {
		return path, true, nil
	}
	parent, name, ok, err := r.hop(nid)
	if err != nil || !ok {
		return "", false, err
	}
	path := name
	if parent.Valid {
		above, ok, err := r.Path(parent.Int64)
		if err != nil || !ok {
			return "", ok, err
		}
		// Only the root has an empty name, and it contributes no segment.
		if above != "" {
			path = above + "/" + name
		}
	}
	r.memo[nid] = path
	return path, true, nil
}

// ParsedSourceLike reports whether any source file whose id matches the SQL
// LIKE pattern has a parse tree in _ast: whether that language was parsed at
// all. projection-v4 records each _ast row's file; projection-v6 keys _ast by
// nid, so a file's rows are the nid range its file_id names (V6FileNids).
// False, without error, when the db has no _ast.
func ParsedSourceLike(qg Querier, pattern string) (bool, error) {
	v6, err := TableHasColumn(qg, "_ast", "nid")
	if err != nil {
		return false, err
	}
	q := `SELECT EXISTS(SELECT 1 FROM _ast WHERE source_id LIKE ?)`
	if v6 {
		shift := strconv.Itoa(V6FileShift)
		q = `SELECT EXISTS(SELECT 1 FROM _source s WHERE s.id LIKE ? AND EXISTS(
		       SELECT 1 FROM _ast a
		       WHERE a.nid >= s.file_id << ` + shift + ` AND a.nid < (s.file_id + 1) << ` + shift + `))`
	} else if has, err := TableHasColumn(qg, "_ast", "source_id"); err != nil || !has {
		return false, err
	}
	rows, err := qg.QueryRefs(q, pattern)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var exists bool
	if rows.Next() {
		if err := rows.Scan(&exists); err != nil {
			return false, err
		}
	}
	return exists, rows.Err()
}
