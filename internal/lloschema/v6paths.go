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
	db                           *sql.DB
	path, file, dir, single, nth *sqlstmt.Lazy
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
	}
}

// Close releases the compiled statements. It does not close the db.
func (p *V6Paths) Close() {
	for _, s := range []*sqlstmt.Lazy{p.path, p.file, p.dir, p.single, p.nth} {
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
