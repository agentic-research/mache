package graph

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLiteGraph_RefsConnUsesQueryRefsDatabase(t *testing.T) {
	open := func(name string) *sql.DB {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		_, err = db.Exec("CREATE TABLE marker (value TEXT); INSERT INTO marker VALUES (?)", name)
		require.NoError(t, err)
		return db
	}
	g := &SQLiteGraph{db: open("main"), refsDB: open("sidecar")}
	for _, nodes := range []bool{true, false} {
		g.useNodesTable = nodes
		conn, err := g.RefsConn(context.Background())
		require.NoError(t, err)
		var got string
		require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT value FROM marker").Scan(&got))
		want := "sidecar"
		if nodes {
			want = "main"
		}
		require.Equal(t, want, got)
		require.NoError(t, conn.Close())
	}
}
