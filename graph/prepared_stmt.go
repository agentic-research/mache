package graph

import (
	"database/sql"
	"sync"
	"sync/atomic"
)

// preparedStmt is a statement compiled on first use and reused thereafter
// (mache-3063fb).
//
// The direct read path used to hand database/sql a query STRING on every call,
// so modernc compiled the same SQL from scratch and discarded it each time —
// about half of a warm point read once page I/O was out of the way.
// TestNodesTableReader_CompilesEachStatementOnce pins the fix as a count.
//
// LAZY, deliberately. A reader is constructed against dbs that do not carry
// every table it can query — a standalone mache projection has no node_refs —
// and preparing eagerly would fail the constructor where today's code only
// errors when GetCallers is actually called. Preparing on first use keeps that
// behaviour exactly: the same call fails, at the same moment, with the same
// error.
//
// A FAILED prepare is not cached. sync.Once would make one transient failure
// permanent for the life of the reader; here the next call simply tries again.
// The fast path is a single atomic load, so the lock is only ever taken by
// callers racing to compile the statement the first time.
type preparedStmt struct {
	query string
	mu    sync.Mutex
	stmt  atomic.Pointer[sql.Stmt]
}

func newPreparedStmt(query string) *preparedStmt { return &preparedStmt{query: query} }

// get returns the compiled statement, compiling it on first use.
func (p *preparedStmt) get(db *sql.DB) (*sql.Stmt, error) {
	if s := p.stmt.Load(); s != nil {
		return s, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.stmt.Load(); s != nil {
		return s, nil
	}
	s, err := db.Prepare(p.query)
	if err != nil {
		return nil, err
	}
	p.stmt.Store(s)
	return s, nil
}

// close releases the statement if one was compiled. Safe to call on a
// statement that was never used.
func (p *preparedStmt) close() {
	if s := p.stmt.Swap(nil); s != nil {
		_ = s.Close()
	}
}
