// Package sqlstmt holds a statement compiled on first use and reused
// thereafter (mache-3063fb).
//
// It is its own package because two readers need it and neither may import
// the other: graph's NodesTableReader, and lloschema's projection-v6 path
// addressing, which graph imports.
package sqlstmt

import (
	"database/sql"
	"sync"
	"sync/atomic"
)

// Lazy is a statement compiled on first use and reused thereafter.
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
type Lazy struct {
	query string
	mu    sync.Mutex
	stmt  atomic.Pointer[sql.Stmt]
}

// NewLazy returns a statement for query, not yet compiled.
func NewLazy(query string) *Lazy { return &Lazy{query: query} }

// Get returns the compiled statement, compiling it on first use.
func (p *Lazy) Get(db *sql.DB) (*sql.Stmt, error) {
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

// Close releases the statement if one was compiled. Safe to call on a
// statement that was never used.
func (p *Lazy) Close() {
	if s := p.stmt.Swap(nil); s != nil {
		_ = s.Close()
	}
}
