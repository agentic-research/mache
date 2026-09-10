// Package sqlcount counts SQL statements, so a test can assert how work
// GROWS rather than how long it takes.
//
// It lives in its own package because internal/testutil imports
// internal/ingest, and the walker's own tests need this.
package sqlcount

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"
)

// SQL QUERY counting for growth-class assertions.
//
// The point is to measure WORK in a machine-independent unit. mache already
// shipped an O(nodes²) projection regression through green CI (mache-4f3840),
// and wall-clock cannot tell a slow runner from a wrong complexity class —
// while a statement count can. It also has no flake surface, which timing
// gates in this repo demonstrably do.
//
// The wrapper deliberately implements ONLY driver.Conn. database/sql promotes
// a connection to the fast path when it satisfies driver.QueryerContext, and
// an embedded interface field does not carry that through a type assertion —
// so every statement is forced down Prepare + Stmt, where exactly one count is
// recorded per execution. Implementing both paths would double-count some
// statements and not others, which is worse than counting nothing.

var (
	countingOnce sync.Once
	sqlQueries   atomic.Int64
)

// Only QUERIES are counted, not writes.
//
// A query count is what separates O(n) from O(n²) on a read path: the
// regression this exists to catch (mache-4f3840) was a per-scope SELECT loop.
// Counting writes as well would mean implementing the exec half of
// database/sql's dual interface, whose methods are structurally identical to
// the query half and differ only in return type — real duplication, reported
// as such by duplicate_code, bought for a number no gate currently reads.
//
// Writes pass through the embedded statement untouched. When a write-path gate
// needs them, add the exec family then, with a use for the number.

// DriverName is the driver to open a database with when the test wants
// its statements counted. Call Register first.
const DriverName = "sqlite-counting"

// RegisterDriver registers the counting driver, once per process.
//
// The base driver is taken from an opened "sqlite" handle rather than
// constructed, so this stays correct if the sqlite package changes how it
// exports its driver.
func RegisterDriver() {
	countingOnce.Do(func() {
		probe, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic("testutil: open sqlite to borrow its driver: " + err.Error())
		}
		base := probe.Driver()
		_ = probe.Close()
		sql.Register(DriverName, &countingDriver{base: base})
	})
}

// Reset zeroes the counter and returns a func reading it.
//
// The counter is process-global, so a test that reads it must not run in
// parallel with another that issues statements.
func Reset() (count func() int64) {
	sqlQueries.Store(0)
	return sqlQueries.Load
}

type countingDriver struct{ base driver.Driver }

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c}, nil
}

// countingConn intentionally exposes only driver.Conn — see the file comment.
type countingConn struct{ driver.Conn }

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.Conn.Prepare(query)
	return wrapStmt(s, err)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	s, err := pc.PrepareContext(ctx, query)
	return wrapStmt(s, err)
}

// wrapStmt resolves the context interfaces ONCE, when the statement is
// created, and refuses the statement if either is missing.
//
// Checking per execution would repeat the same branch down four paths and put
// it in the hot path; checking here fails loudly at the only moment the answer
// can change. A wrapper that silently fell back to the deprecated methods
// would count nothing on that path while appearing to work, which is the one
// outcome worse than not counting at all.
func wrapStmt(s driver.Stmt, err error) (driver.Stmt, error) {
	if err != nil {
		return nil, err
	}
	qc, ok := s.(driver.StmtQueryContext)
	if !ok {
		_ = s.Close()
		return nil, fmt.Errorf("sqlcount: wrapped statement lacks StmtQueryContext; " +
			"counting would silently miss executions")
	}
	return &countingStmt{Stmt: s, query: qc}, nil
}

type countingStmt struct {
	driver.Stmt
	query driver.StmtQueryContext
}

func (s *countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	sqlQueries.Add(1)
	return s.query.QueryContext(ctx, args)
}

// Query routes into QueryContext rather than into the wrapped statement, so
// either entry point database/sql might choose counts exactly once and no
// deprecated driver method is called downstream.
func (s *countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}
