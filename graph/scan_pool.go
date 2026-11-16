package graph

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
)

// scanPoolSize is how many connections long scans may hold at once on one
// database. A scan that needs one more waits for a scan to finish, or for its
// context to end.
const scanPoolSize = 2

// errNoRefsDB is what RefsConn returns on a backend with no refs database.
var errNoRefsDB = errors.New("refs database not initialized")

// errScanPoolClosed is what RefsConn returns once its graph is closed.
var errScanPoolClosed = errors.New("graph closed: no connection to reserve")

// scanPool is the second pool on a database, from which RefsConn reserves.
//
// A reservation is held for a whole analytical run: find_smells keeps its
// TEMP vocabulary on one connection through setup, preflight, every rule and
// enrichment, which takes about a minute on a large repository. Drawn from
// the pool navigation uses (find_definition, list_directory, read_file...),
// a few concurrent scans would hold every connection, and every other tool
// on the graph would wait behind them (mache-574dd5). The scan pool is the
// same file under the same DSN, so a scan reads exactly what navigation
// reads; it is opened on first use and closed with its graph.
type scanPool struct {
	dsn  string
	once sync.Once
	db   atomic.Pointer[sql.DB] // set once, by the first conn
	err  error
}

func newScanPool(dsn string) *scanPool { return &scanPool{dsn: dsn} }

// RefsConn implements RefsConnectionProvider for the backends that embed
// the pool: it reserves a connection, waiting while scanPoolSize are held.
// A backend with no refs database has no pool, and refuses.
func (p *scanPool) RefsConn(ctx context.Context) (*sql.Conn, error) {
	if p == nil {
		return nil, errNoRefsDB
	}
	p.once.Do(func() {
		db, err := sql.Open("sqlite", p.dsn)
		if err != nil {
			p.err = err
			return
		}
		db.SetMaxOpenConns(scanPoolSize)
		p.db.Store(db)
	})
	if p.err != nil {
		return nil, p.err
	}
	return p.db.Load().Conn(ctx)
}

// stats reports the pool's state, and false if no scan has opened it.
func (p *scanPool) stats() (sql.DBStats, bool) {
	db := p.db.Load()
	if db == nil {
		return sql.DBStats{}, false
	}
	return db.Stats(), true
}

// closeScans closes the pool and refuses later reservations. Reservations
// still held keep their connection until released, as with any *sql.DB. A
// backend with no pool has nothing to close.
func (p *scanPool) closeScans() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() { p.err = errScanPoolClosed })
	if db := p.db.Load(); db != nil {
		return db.Close()
	}
	return nil
}
