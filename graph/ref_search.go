package graph

import (
	"database/sql"
	"fmt"
	"strings"
)

// RefHit is one reference search result: the token referenced and the node
// that references it.
type RefHit struct {
	Token string
	ID    string
}

// RefSearcher answers `search role=reference`: references whose token
// matches a SQL LIKE pattern, optionally only those whose node sits under a
// path segment, at most limit of them. Engine sentinels (FileLevelSentinelPrefix)
// are never hits, as GetCallers does not return them either.
//
// It is a capability rather than SQL in the handler because where references
// live depends on the backend: the mache_refs sidecar for MemoryStore and the
// legacy path, node_refs for a nodes-table .db, keyed by node_id on
// projection-v4 and by nid on projection-v6 (mache-90cb9f).
type RefSearcher interface {
	SearchRefs(pattern, segment string, limit int) ([]RefHit, error)
}

var (
	_ RefSearcher = (*MemoryStore)(nil)
	_ RefSearcher = (*SQLiteGraph)(nil)
)

// SearchRefs implements [RefSearcher] over the mache_refs sidecar.
func (s *MemoryStore) SearchRefs(pattern, segment string, limit int) ([]RefHit, error) {
	if s.refsDB == nil {
		return nil, fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}
	return searchMacheRefs(s.refsDB, pattern, segment, limit)
}

// SearchRefs implements [RefSearcher].
func (g *SQLiteGraph) SearchRefs(pattern, segment string, limit int) ([]RefHit, error) {
	if !g.useNodesTable {
		return searchMacheRefs(g.refsDB, pattern, segment, limit)
	}
	if g.shape() == nil {
		// A v4 id is its path, so the segment and sentinel filters are SQL.
		q := "SELECT token, node_id FROM node_refs WHERE token LIKE ? AND node_id NOT LIKE '" + sentinelSQLPattern + "'"
		args := []any{pattern}
		if segment != "" {
			q += " AND node_id LIKE ?"
			args = append(args, "%/"+segment+"/%")
		}
		pairs, err := g.queryTokenIDs(q+" LIMIT ?", append(args, limit)...)
		return hits(pairs), err
	}
	// v6: the path exists only once rendered, so a segment filter runs on
	// rendered paths and the limit after it. LLO writes no sentinels.
	q := "SELECT token, nid FROM node_refs WHERE token LIKE ?"
	args := []any{pattern}
	if segment == "" {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	pairs, err := g.queryTokenIDs(q, args...)
	if err != nil {
		return nil, err
	}
	var out []RefHit
	for _, p := range pairs {
		if segment != "" && !strings.Contains(p.id, "/"+segment+"/") {
			continue
		}
		out = append(out, RefHit{Token: p.token, ID: p.id})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func hits(pairs []tokenID) []RefHit {
	out := make([]RefHit, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, RefHit{Token: p.token, ID: p.id})
	}
	return out
}

// searchMacheRefs searches the mache_refs sidecar, whose path column is a
// node id by construction.
func searchMacheRefs(db *sql.DB, pattern, segment string, limit int) ([]RefHit, error) {
	q := "SELECT token, path FROM mache_refs WHERE token LIKE ? AND path NOT LIKE '" + sentinelSQLPattern + "'"
	args := []any{pattern}
	if segment != "" {
		q += " AND path LIKE ?"
		args = append(args, "%/"+segment+"/%")
	}
	rows, err := db.Query(q+" LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RefHit
	for rows.Next() {
		var h RefHit
		if err := rows.Scan(&h.Token, &h.ID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
