package smells

import (
	"context"
	"database/sql"

	"github.com/agentic-research/mache/graph"
)

// pinSmellConnection keeps setup, preflight, and execution on one connection
// without changing the shared graph pool. Already-pinned and single-connection
// queriers can continue to implement only RefsQuerier.
func pinSmellConnection(ctx context.Context, qg graph.RefsQuerier) (graph.RefsQuerier, func(), error) {
	p, ok := qg.(graph.RefsConnectionProvider)
	if !ok {
		return qg, func() {}, nil
	}
	conn, err := p.RefsConn(ctx)
	if err != nil {
		return nil, nil, err
	}
	path := ""
	if dp, ok := qg.(graph.DBPathProvider); ok {
		path = dp.DBPath()
	}
	return &smellConnection{conn: conn, ctx: ctx, path: path}, func() { _ = conn.Close() }, nil
}

type smellConnection struct {
	conn *sql.Conn
	ctx  context.Context
	path string
}

func (q *smellConnection) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.conn.QueryContext(q.ctx, query, args...)
}

func (q *smellConnection) DBPath() string { return q.path }
