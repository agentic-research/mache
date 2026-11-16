package graph

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/stretchr/testify/require"
)

// scanBackend is one graph backend that serves both navigation (QueryRefs)
// and long scans (RefsConn).
type scanBackend struct {
	name string
	// navPool is how many connections the pool QueryRefs uses holds.
	navPool int
	open    func(t *testing.T) (refsBackend, *scanPool)
}

type refsBackend interface {
	RefsQuerier
	RefsConnectionProvider
	Close() error
}

func scanBackends() []scanBackend {
	return []scanBackend{
		{"SQLiteGraph/nodes", 4, func(t *testing.T) (refsBackend, *scanPool) {
			g := graphWithOneDef(t)
			return g, g.scanPool
		}},
		// How mache serve opens the parse db it owns (internal/mcpserve).
		{"SQLiteGraph/nodes/LockForLife", 4, func(t *testing.T) (refsBackend, *scanPool) {
			path, _ := fixturedb.New(t, fixturedb.Leyline).Def("Run", "alpha.go/function_declaration_0", "function").Build()
			g, err := OpenSQLiteGraphWith(path, &api.Topology{}, machetmpl.Render, LockForLife)
			require.NoError(t, err)
			t.Cleanup(func() { _ = g.Close() })
			return g, g.scanPool
		}},
		{"SQLiteGraph/legacy", 2, func(t *testing.T) (refsBackend, *scanPool) {
			g, err := OpenSQLiteGraph(createTestDB(t, map[string]string{"r1": `{"id":"r1"}`}), kevSchema(), testRender)
			require.NoError(t, err)
			t.Cleanup(func() { _ = g.Close() })
			return g, g.scanPool
		}},
		{"MemoryStore", 2, func(t *testing.T) (refsBackend, *scanPool) {
			s := NewMemoryStore()
			require.NoError(t, s.InitRefsDB())
			t.Cleanup(func() { _ = s.Close() })
			return s, s.scanPool
		}},
	}
}

// mainFile is the file a query on q's main schema reads.
func mainFile(t *testing.T, query func(string) *sql.Row) string {
	t.Helper()
	var file string
	require.NoError(t, query(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file))
	return file
}

// A scan reads the database navigation reads: the same file, so its findings
// are about the graph being served.
func TestRefsConn_ReadsTheDatabaseQueryRefsReads(t *testing.T) {
	for _, b := range scanBackends() {
		t.Run(b.name, func(t *testing.T) {
			g, _ := b.open(t)
			ctx := context.Background()
			conn, err := g.RefsConn(ctx)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()

			rows, err := g.QueryRefs(`SELECT file FROM pragma_database_list WHERE name = 'main'`)
			require.NoError(t, err)
			require.True(t, rows.Next())
			var want string
			require.NoError(t, rows.Scan(&want))
			require.NoError(t, rows.Close())

			got := mainFile(t, func(q string) *sql.Row { return conn.QueryRowContext(ctx, q) })
			require.NotEmpty(t, want)
			require.Equal(t, want, got)
		})
	}
}

// Many agents scanning one served graph at once must not stall the rest of
// its tools (mache-574dd5). A scan holds its reservation for a whole
// find_smells run, a minute on a large repository; drawn from navigation's
// pool, a few concurrent scans held every connection and find_definition,
// list_directory and the rest waited behind them.
//
// As many scans as navigation's pool has connections are reserved and held;
// navigation must still answer at once. Drawn from the navigation pool, the
// scans hold all of it and the query waits past its deadline.
func TestRefsConn_ScansNeverStarveNavigation(t *testing.T) {
	for _, b := range scanBackends() {
		t.Run(b.name, func(t *testing.T) {
			g, scans := b.open(t)
			ctx, cancel := context.WithCancel(context.Background())
			type reservation struct {
				conn *sql.Conn
				err  error
			}
			got := make(chan reservation, b.navPool)
			var acquired atomic.Int64
			for range b.navPool {
				go func() {
					c, err := g.RefsConn(ctx)
					if err == nil {
						acquired.Add(1)
					}
					got <- reservation{c, err}
				}()
			}
			// Settled: each reservation either holds a connection or waits
			// on the scan pool.
			require.Eventually(t, func() bool {
				st, _ := scans.stats()
				return acquired.Load()+st.WaitCount == int64(b.navPool)
			}, 10*time.Second, time.Millisecond, "reservations never settled")
			held := make([]*sql.Conn, 0, acquired.Load())
			for range acquired.Load() {
				r := <-got
				require.NoError(t, r.err)
				held = append(held, r.conn)
			}

			answered := make(chan error, 1)
			go func() {
				rows, err := g.QueryRefs(`SELECT count(*) FROM sqlite_master`)
				if err == nil {
					err = rows.Close()
				}
				answered <- err
			}()
			deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			select {
			case err := <-answered:
				require.NoError(t, err)
			case <-deadline.Done():
				t.Fatalf("navigation waited behind %d scans holding its connections", len(held))
			}
			require.Len(t, held, scanPoolSize, "scans beyond the scan pool must queue")

			// Scans cancelled while queued give up with their context's
			// error and take no connection with them.
			cancel()
			for range b.navPool - len(held) {
				r := <-got
				require.ErrorIs(t, r.err, context.Canceled)
			}
			for _, c := range held {
				require.NoError(t, c.Close())
			}
			st, _ := scans.stats()
			require.Zero(t, st.InUse, "a scan connection leaked")
		})
	}
}

// Closing the graph closes its scan pool: a later reservation is refused,
// not served from a pool nothing will close.
func TestRefsConn_RefusedAfterClose(t *testing.T) {
	for _, b := range scanBackends() {
		t.Run(b.name, func(t *testing.T) {
			g, scans := b.open(t)
			c, err := g.RefsConn(context.Background())
			require.NoError(t, err)
			require.NoError(t, c.Close())
			require.NoError(t, g.Close())

			_, err = g.RefsConn(context.Background())
			require.Error(t, err)
			require.Error(t, scans.db.Load().Ping(), "the scan pool is still open after Close")
		})
	}
}

// A graph closed before any scan never opens its scan pool.
func TestScanPool_ClosedBeforeUseNeverOpens(t *testing.T) {
	p := newScanPool(":memory:")
	require.NoError(t, p.closeScans())
	_, err := p.RefsConn(context.Background())
	require.True(t, errors.Is(err, errScanPoolClosed), "got %v", err)
	_, opened := p.stats()
	require.False(t, opened)
}
