package graph

import (
	"strconv"

	"github.com/agentic-research/mache/internal/lloschema"
	"github.com/agentic-research/mache/internal/sqlstmt"
)

// The nodes table comes in two shapes, and the reader serves both through one
// set of methods.
//
//   - Path-keyed: mache's own projection and ley-line-open up to
//     projection-v4. A node's id IS its path: nodes.id, parent_id, name.
//   - nid-keyed: ley-line-open's projection-v6 (v0.20.x). A node is an
//     integer nid with a parent_nid, and its path is derived, never stored
//     (mache-90cb9f).
//
// The rows carry the same facts either way: kind, size, mtime, record,
// record_id, and a source location in _ast. What differs is how a path
// becomes the row key, and how a row becomes a path. That is all v6Shape
// holds; every method of the reader is otherwise shared.

// v6Shape addresses a projection-v6 nodes table. A nil *v6Shape is the
// path-keyed shape.
type v6Shape struct{ paths *lloschema.V6Paths }

// newV6Shape returns the nid-keyed shape when the db's nodes table has one, and
// nil otherwise. The column is the test, not a declared version: the reader
// already decides every other column by probing (see ColumnExists).
func newV6Shape(r *NodesTableReader) *v6Shape {
	if !ColumnExists(r.db, "nodes", "nid") {
		return nil
	}
	return &v6Shape{paths: lloschema.NewV6Paths(r.db)}
}

// key turns a path into what the row statements are keyed by: the path itself,
// or its nid. ok is false when no node renders as path.
func (s *v6Shape) key(id string) (any, bool, error) {
	if s == nil {
		return id, true, nil
	}
	return s.paths.Resolve(id)
}

// childID is the id of a child whose listing column is col: on v6 the
// statement returns the child's NAME, and its path is the parent's plus that.
func (s *v6Shape) childID(parent, col string) string {
	if s == nil || parent == "" {
		return col
	}
	return parent + "/" + col
}

// ids turns node-id columns, as node_defs, node_refs or nodes return them,
// into node ids (see idFunc). A column naming no node is dropped, as a
// dangling row names nothing a consumer can open.
func (s *v6Shape) ids(cols []any) ([]string, error) {
	id := s.idFunc()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		v, ok, err := id(c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// idFunc returns the conversion from one node-id column to a node id: the
// column itself, or the path of the nid it holds. On v6 it shares one
// renderer across every call, so a batch names each shared ancestor once;
// take a fresh one per batch.
func (s *v6Shape) idFunc() func(col any) (string, bool, error) {
	if s == nil {
		return func(col any) (string, bool, error) {
			id, ok := col.(string)
			return id, ok, nil
		}
	}
	r := s.paths.Renderer()
	return func(col any) (string, bool, error) {
		nid, ok := col.(int64)
		if !ok {
			return "", false, nil
		}
		return r.Path(nid)
	}
}

// The columns node_defs and node_refs name nodes by. projection-v6 renamed
// them and dropped node_refs.source_id: a reference's file is its site's,
// found through the nid's file id.
func (s *v6Shape) defNode() string { return s.pick("node_id", "nid") }

func (s *v6Shape) refSite() string { return s.pick("node_id", "nid") }

func (s *v6Shape) refContainer() string { return s.pick("container_node_id", "container_nid") }

func (s *v6Shape) refSource() string {
	return s.pick("source_id", "(SELECT id FROM _source WHERE file_id = nid >> "+strconv.Itoa(lloschema.V6FileShift)+")")
}

func (s *v6Shape) pick(v4, v6 string) string {
	if s == nil {
		return v4
	}
	return v6
}

func (s *v6Shape) close() {
	if s != nil {
		s.paths.Close()
	}
}

// statements are the reader's statements for this shape. Both shapes return
// the same columns in the same order, so scanning is shared.
func (s *v6Shape) statements(r *NodesTableReader) readerStmts {
	if s == nil {
		return readerStmts{
			node:         sqlstmt.NewLazy(r.nodeSelect()),
			children:     sqlstmt.NewLazy("SELECT id FROM nodes WHERE parent_id = ? ORDER BY name"),
			childrenRoot: sqlstmt.NewLazy("SELECT id FROM nodes WHERE (parent_id = '' OR parent_id IS NULL) AND id != '' ORDER BY name"),
			stats:        sqlstmt.NewLazy("SELECT id, kind, size, mtime FROM nodes WHERE parent_id = ? ORDER BY name"),
			statsRoot:    sqlstmt.NewLazy("SELECT id, kind, size, mtime FROM nodes WHERE (parent_id = '' OR parent_id IS NULL) AND id != '' ORDER BY name"),
			content:      sqlstmt.NewLazy("SELECT record, record_id FROM nodes WHERE id = ?"),
			record:       sqlstmt.NewLazy("SELECT record FROM " + r.tableName + " WHERE id = ?"),
			callers: sqlstmt.NewLazy(
				"SELECT node_id FROM node_refs WHERE token = ? AND node_id NOT LIKE '" + sentinelSQLPattern + "'"),
		}
	}
	// A child's name comes from LLO's v_node_name, so the naming rule has one
	// owner. The root is the directory whose nid is fixed by the layout, so
	// its listing needs no lookup.
	root := strconv.Itoa(lloschema.V6RootNid)
	return readerStmts{
		node: sqlstmt.NewLazy(r.nodeSelect()),
		children: sqlstmt.NewLazy(`SELECT v.name FROM nodes n JOIN v_node_name v ON v.nid = n.nid
WHERE n.parent_nid = ? ORDER BY v.name`),
		childrenRoot: sqlstmt.NewLazy(`SELECT v.name FROM nodes n JOIN v_node_name v ON v.nid = n.nid
WHERE n.parent_nid = ` + root + ` ORDER BY v.name`),
		stats: sqlstmt.NewLazy(`SELECT v.name, n.kind, n.size, n.mtime FROM nodes n JOIN v_node_name v ON v.nid = n.nid
WHERE n.parent_nid = ? ORDER BY v.name`),
		statsRoot: sqlstmt.NewLazy(`SELECT v.name, n.kind, n.size, n.mtime FROM nodes n JOIN v_node_name v ON v.nid = n.nid
WHERE n.parent_nid = ` + root + ` ORDER BY v.name`),
		content: sqlstmt.NewLazy("SELECT record, record_id FROM nodes WHERE nid = ?"),
		record:  sqlstmt.NewLazy("SELECT record FROM " + r.tableName + " WHERE id = ?"),
		// The site of each reference, as projection-v4's node_refs.node_id is.
		callers: sqlstmt.NewLazy("SELECT nid FROM node_refs WHERE token = ?"),
	}
}

// nodeKeyColumn and astJoin are where nodeSelect differs by shape. projection-v6
// dropped _ast.source_id; a node's file is its nid's top bits, and _source
// maps that file_id back to the source id a v4 row would have carried.
func (s *v6Shape) nodeKeyColumn() string {
	if s == nil {
		return "n.id"
	}
	return "n.nid"
}

func (s *v6Shape) astJoin() (sourceCol, join string) {
	if s == nil {
		return "a.source_id", " LEFT JOIN _ast a ON a.node_id = n.id"
	}
	return "s.id", " LEFT JOIN _ast a ON a.nid = n.nid LEFT JOIN _source s ON s.file_id = a.nid >> " +
		strconv.Itoa(lloschema.V6FileShift)
}
