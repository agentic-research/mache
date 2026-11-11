package graph

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/internal/lloschema"
)

var _ ContentHasher = (*SQLiteGraph)(nil)

// ParserID implements [ContentHasher]: the parserId of _meta.derived_from.
func (g *SQLiteGraph) ParserID() (string, error) {
	return lloschema.ParserID(g)
}

// nodeHashBatch is how many node keys one NodeHashes query names. SQLite's
// default parameter limit is 32766; each key is bound once per arm.
const nodeHashBatch = 500

// NodeHashes implements [ContentHasher]. A node's hash is its _ast row's,
// through lloschema.NodeHashExpr so projection-v10's content_id indirection
// reads the same as a stored column. An occurrence ley-line numbers in a host
// file but stores no node for (an injected language, ley-line-open-260fcf) has
// no _ast row; its node_hash is on its node_refs row, which carries the same
// content_id an _ast row would (0 disagreements on a v0.29.0 parse of this
// repo), so the refs table is the second arm of the one query. A producer
// that wrote no node_hash on either table (mache's own projection) yields an
// empty map.
func (g *SQLiteGraph) NodeHashes(ids []string) (map[string][32]byte, error) {
	out := make(map[string][32]byte, len(ids))
	astHash, hasAST, err := lloschema.NodeHashExpr(g, "_ast", "a")
	if err != nil {
		return nil, err
	}
	refHash, hasRefs, err := lloschema.NodeHashExpr(g, "node_refs", "r")
	if err != nil {
		return nil, err
	}
	if !hasAST && !hasRefs {
		return out, nil
	}

	s := g.shape()
	var arms []string
	if hasAST {
		arms = append(arms, "SELECT a."+s.pick("node_id", "nid")+" AS k, "+astHash+" AS h FROM _ast a WHERE a."+
			s.pick("node_id", "nid")+" IN (%s)")
	}
	if hasRefs {
		arms = append(arms, "SELECT r."+s.refSite()+", "+refHash+" FROM node_refs r WHERE r."+s.refSite()+" IN (%s)")
	}

	// A path resolves to the key node_defs and node_refs name it by (its nid
	// on projection-v6), and two paths cannot share a key.
	idOf := make(map[any]string, len(ids))
	keys := make([]any, 0, len(ids))
	for _, id := range ids {
		k, ok, err := g.key(id)
		if err != nil {
			return nil, fmt.Errorf("node hash of %s: %w", id, err)
		}
		if !ok {
			continue
		}
		if _, seen := idOf[k]; seen {
			continue
		}
		idOf[k] = id
		keys = append(keys, k)
	}

	for start := 0; start < len(keys); start += nodeHashBatch {
		batch := keys[start:min(start+nodeHashBatch, len(keys))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch)*len(arms))
		filled := make([]string, len(arms))
		for i, arm := range arms {
			filled[i] = strings.Replace(arm, "%s", marks, 1)
			args = append(args, batch...)
		}
		if err := g.scanNodeHashes(strings.Join(filled, " UNION "), args, idOf, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanNodeHashes reads (key, hash) rows into out. A NULL hash is a producer
// that wrote none for that row. Two different hashes for one key is a
// database that contradicts itself, not a choice to make.
func (g *SQLiteGraph) scanNodeHashes(query string, args []any, idOf map[any]string, out map[string][32]byte) error {
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("node hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k any
		var h []byte
		if err := rows.Scan(&k, &h); err != nil {
			return fmt.Errorf("node hashes: %w", err)
		}
		if h == nil {
			continue
		}
		id, ok := idOf[k]
		if !ok {
			return fmt.Errorf("node hashes: the database returned key %v for no asked node", k)
		}
		if len(h) != 32 {
			return fmt.Errorf("node hash of %s: %d bytes, want 32", id, len(h))
		}
		if prev, dup := out[id]; dup && !bytes.Equal(prev[:], h) {
			return fmt.Errorf("node hash of %s: _ast and node_refs disagree (%x vs %x)", id, prev, h)
		}
		out[id] = [32]byte(h)
	}
	return rows.Err()
}
