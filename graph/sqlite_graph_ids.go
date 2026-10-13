package graph

import (
	"fmt"

	"github.com/agentic-research/mache/internal/lloschema"
)

// Every SQLiteGraph lookup that reads node ids out of node_defs or node_refs
// goes through these, so it reads projection-v4 and projection-v6 alike: the
// query names its columns through the reader's shape (see nodes_shape.go),
// and the ids it returns are converted here. A query that spelled node_id
// itself would fail on v6, and the callers below that swallow errors would
// turn that into an empty answer (mache-90cb9f).

// shape is the reader's: nil for a path-keyed nodes table, and for the
// legacy no-nodes-table path, which only path-keyed producers write.
func (g *SQLiteGraph) shape() *v6Shape {
	if g.ntr == nil {
		return nil
	}
	return g.ntr.shape
}

// key turns a node id into what node_defs and node_refs name it by.
func (g *SQLiteGraph) key(id string) (any, bool, error) { return g.shape().key(id) }

// queryIDs runs a query whose one column is a node id and returns the ids.
// Rows are read in full before converting: on v6 a conversion is a query of
// its own, which must not wait on this one's connection.
func (g *SQLiteGraph) queryIDs(query string, args ...any) ([]string, error) {
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var cols []any
	for rows.Next() {
		var c any
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return g.shape().ids(cols)
}

// tokenID is one (token, node id) row of node_defs or node_refs.
type tokenID struct{ token, id string }

// queryTokenIDs runs a query returning (token, node id) and returns the
// pairs, dropping a row whose node does not exist.
func (g *SQLiteGraph) queryTokenIDs(query string, args ...any) ([]tokenID, error) {
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	type raw struct {
		token string
		col   any
	}
	var all []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.token, &r.col); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan token/node row: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	id := g.shape().idFunc()
	out := make([]tokenID, 0, len(all))
	for _, r := range all {
		v, ok, err := id(r.col)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, tokenID{r.token, v})
		}
	}
	return out, nil
}

var _ ASTKinder = (*SQLiteGraph)(nil)

// ASTKindChain implements [ASTKinder]: one recursive walk up the nodes table,
// joined to _ast through lloschema's ASTSelect so the kinds read the same on
// either projection. Depth is capped at 256, far past any real nesting, so a
// cyclic parent chain in a malformed table cannot spin forever.
func (g *SQLiteGraph) ASTKindChain(id string) ([]string, bool, error) {
	body, err := lloschema.ASTSelect(lloschema.DB(g.db))
	if err != nil || body == "" {
		return nil, false, err
	}
	key, ok, err := g.key(id)
	if err != nil || !ok {
		return nil, false, err
	}
	s := g.shape()
	node, parent := s.pick("id", "nid"), s.pick("parent_id", "parent_nid")
	rows, err := g.db.Query(`
		WITH RECURSIVE up(id, depth) AS (
			SELECT ?, 0
			UNION ALL
			SELECT n.`+parent+`, up.depth + 1 FROM nodes n JOIN up ON n.`+node+` = up.id
			WHERE n.`+parent+` IS NOT NULL AND n.`+parent+` != '' AND up.depth < 256
		)
		SELECT up.depth, a.node_kind FROM up JOIN (`+body+`) a ON a.node_id = up.id
		ORDER BY up.depth`, key)
	if err != nil {
		return nil, false, fmt.Errorf("ast kinds of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var chain []string
	self := false
	for rows.Next() {
		var depth int
		var kind string
		if err := rows.Scan(&depth, &kind); err != nil {
			return nil, false, fmt.Errorf("ast kinds of %s: %w", id, err)
		}
		self = self || depth == 0
		chain = append(chain, kind)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if !self {
		return nil, false, nil
	}
	return chain, true, nil
}
