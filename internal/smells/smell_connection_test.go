package smells

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/require"
)

// scanOf opens a smell scan over qg for the rest of the test.
func scanOf(t testing.TB, qg graph.RefsQuerier) *smellScan {
	t.Helper()
	s, err := openSmellScan(context.Background(), qg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pooledSmellFixture(t *testing.T) *testutil.SmellTestGraph {
	t.Helper()
	path, f := fixturedb.New(t, fixturedb.Standalone).
		Construct("pkg/Dead", fixturedb.Where{Source: "dead.go"}).
		Def("Dead", "pkg/Dead", "").Build()
	f.DB().SetMaxOpenConns(4)
	f.DB().SetMaxIdleConns(4)
	return &testutil.SmellTestGraph{MemoryStore: graph.NewMemoryStore(), DB: f.DB(), Path: path}
}

// switchAtPreflight reserves the setup connection just before preflight. A
// pool-backed query MUST now use a different connection, without sleeps or
// scheduler luck. A pinned scan bypasses QueryRefs and keeps its own connection.
type switchAtPreflight struct {
	*testutil.SmellTestGraph
	held    *sql.Conn
	trigger string
}

func (g *switchAtPreflight) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if g.held == nil && strings.HasPrefix(query, g.trigger) {
		var err error
		g.held, err = g.DB.Conn(context.Background())
		if err != nil {
			return nil, err
		}
	}
	return g.DB.Query(query, args...)
}

func TestFindSmells_PooledConnectionRegression(t *testing.T) {
	for _, rule := range []string{"dead_code", "*"} {
		for _, stage := range []string{"preflight", "query"} {
			t.Run(rule+"/"+stage, func(t *testing.T) {
				trigger := "SELECT name FROM sqlite_master"
				if stage == "query" {
					limit := 200
					if rule == "*" {
						limit = digestScanCap
					}
					trigger = fmt.Sprintf(findRegisteredRule(t, "dead_code").Query, "") + fmt.Sprintf(" LIMIT %d", limit)
				}
				g := &switchAtPreflight{SmellTestGraph: pooledSmellFixture(t), trigger: trigger}
				defer func() {
					if g.held != nil {
						_ = g.held.Close()
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				result, err := MakeFindSmellsHandler(g)(ctx, testutil.MakeRequest(map[string]any{"rule": rule}))
				require.NoError(t, err)
				require.False(t, result.IsError, "%s", testutil.ResultText(t, result))
				var response struct {
					Total   int      `json:"total"`
					Skipped []string `json:"rules_skipped"`
				}
				require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &response))
				require.Positive(t, response.Total)
				require.NotContains(t, response.Skipped, "dead_code")
				require.Equal(t, 4, g.DB.Stats().MaxOpenConnections)
				require.Zero(t, g.DB.Stats().InUse)
			})
		}
	}
}

// Demonstrate the underlying SQLite failure independently of the handler fix.
func TestSmellConnection_TEMPObjectsAreConnectionLocal(t *testing.T) {
	g := pooledSmellFixture(t)
	ctx := context.Background()
	q, err := openSmellScan(ctx, g)
	require.NoError(t, err)
	defer func() { _ = q.Close() }()
	missing, err := missingTables(q, []string{"v_nodes", "v_test_nodes"})
	require.NoError(t, err)
	require.Empty(t, missing)
	second, err := g.DB.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()
	other := &smellScan{conn: second, ctx: ctx, path: g.Path} // never prepared
	missing, err = missingTables(other, []string{"v_nodes", "v_test_nodes"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"v_nodes", "v_test_nodes"}, missing)
	_, err = other.QueryRefs("SELECT * FROM v_test_nodes")
	require.ErrorContains(t, err, "no such table: v_test_nodes")
}

type overlappingSmellGraph struct {
	*testutil.SmellTestGraph
	acquired chan struct{}
	proceed  chan struct{}
}

func (g *overlappingSmellGraph) RefsConn(ctx context.Context) (*sql.Conn, error) {
	conn, err := g.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	g.acquired <- struct{}{}
	select {
	case <-g.proceed:
		return conn, nil
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}
}

func TestFindSmells_OverlappingAndRepeatedRequests(t *testing.T) {
	g := &overlappingSmellGraph{SmellTestGraph: pooledSmellFixture(t), acquired: make(chan struct{}, 2), proceed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, rule := range []string{"dead_code", "*"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := MakeFindSmellsHandler(g)(ctx, testutil.MakeRequest(map[string]any{"rule": rule}))
			if err != nil {
				t.Error(err)
				return
			}
			if result.IsError {
				t.Errorf("scan failed: %+v", result)
			}
		}()
	}
	for range 2 {
		select {
		case <-g.acquired:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Both scans own separate connections; unrelated graph work still proceeds.
	rows, err := g.DB.QueryContext(ctx, "SELECT count(*) FROM nodes")
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	require.Equal(t, 2, g.DB.Stats().InUse)
	close(g.proceed)
	wg.Wait()
	require.Zero(t, g.DB.Stats().InUse)
	for range 10 {
		findings := runSmellRule(t, g.SmellTestGraph, map[string]any{"rule": "dead_code"})
		require.Len(t, findings, 1)
	}
	require.Equal(t, 4, g.DB.Stats().MaxOpenConnections)
	require.Zero(t, g.DB.Stats().InUse)
}

func TestFindSmells_CanceledConnectionReservation(t *testing.T) {
	g := pooledSmellFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := MakeFindSmellsHandler(g)(ctx, testutil.MakeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, testutil.ResultText(t, result), "context canceled")
	require.Zero(t, g.DB.Stats().InUse)
}

// Many agents call find_smells on one served graph at once, more of them than
// the pool has connections. A request holds its scan's connection to the end,
// so a request that asked the pool for a SECOND connection while holding it
// would wait on connections held by requests waiting on it in turn: a
// deadlock. With a pool of ONE, the first such request blocks forever, so
// this fails (on its deadline) if any step of a scan, from the digest, the
// rule SQL and its preflight to the snippet and location reads, leaves it.
func TestFindSmells_ConcurrentRequestsBeyondPoolSize(t *testing.T) {
	g := pooledSmellFixture(t)
	g.DB.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const agents = 16
	var wg sync.WaitGroup
	for i := range agents {
		rule := "dead_code"
		if i%2 == 1 {
			rule = "*"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := MakeFindSmellsHandler(g)(ctx, testutil.MakeRequest(map[string]any{"rule": rule}))
			if err != nil {
				t.Error(err)
				return
			}
			if result.IsError {
				t.Errorf("rule %s: %s", rule, testutil.ResultText(t, result))
			}
		}()
	}
	wg.Wait()
	require.NoError(t, ctx.Err(), "requests deadlocked on the pool")
	require.Zero(t, g.DB.Stats().InUse, "a scan did not return its connection")
}
