package smells

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agentic-research/mache/graph"
)

// smellScan is one smell run's hold on the database: a single reserved
// connection with the TEMP vocabulary (v_ast, v_nodes, v_defs, v_refs,
// v_test_nodes, v_doc_refs, ...) installed on it.
//
// TEMP objects are connection-local. A pooled handle (the server's graph pool
// holds four) can run the setup on one connection and the preflight or the
// rule SQL on another, which sees none of it: "no such table", or a rule
// silently skipped as unsupported (mache-ee4375). Every rule runner takes a
// *smellScan, so the MCP handler, the digest and the CLI all hold the
// connection the vocabulary was installed on, and none can query a pool.
type smellScan struct {
	conn *sql.Conn
	ctx  context.Context
	path string
}

// openSmellScan reserves one connection from qg and installs the smell query
// vocabulary on it, once for the whole run (see ensureSmellQueryContext). The
// caller must Close it to return the connection to its pool.
func openSmellScan(ctx context.Context, qg graph.RefsQuerier) (*smellScan, error) {
	p, ok := qg.(graph.RefsConnectionProvider)
	if !ok {
		return nil, fmt.Errorf("the active graph backend cannot reserve a SQL connection; find_smells requires a leyline-parsed .db")
	}
	conn, err := p.RefsConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve connection: %w", err)
	}
	s := &smellScan{conn: conn, ctx: ctx}
	if dp, ok := qg.(graph.DBPathProvider); ok {
		s.path = dp.DBPath()
	}
	if err := ensureSmellQueryContext(s); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("prepare smell query context: %w", err)
	}
	return s, nil
}

// Close returns the reserved connection to its pool.
func (s *smellScan) Close() error { return s.conn.Close() }

func (s *smellScan) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return s.conn.QueryContext(s.ctx, query, args...)
}

// DBPath implements graph.DBPathProvider, so the capnp readthrough finds the
// sibling .bindings.capnp log of the db this scan reads.
func (s *smellScan) DBPath() string { return s.path }
