package lloschema

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestNodeHashExpr reads each occurrence row's node_hash from wherever this
// producer keeps it. projection-v10 (the LeylineV6 fixture, re-derived from
// the pinned binary) stores content_id on _ast and node_defs and the hash
// lives in node_content; v4 stores the hash on the row; mache's standalone
// node_defs has no hash at all. Every hash read must be the row's
// node_content identity; a v10 db that answered "no hash" would silently key
// smell findings by path (mache-99324b).
func TestNodeHashExpr(t *testing.T) {
	for _, tc := range []struct {
		name     string
		producer fixturedb.Producer
		table    string
		hashed   bool
	}{
		{"projection-v10 _ast", fixturedb.LeylineV6, "_ast", true},
		{"projection-v10 node_defs", fixturedb.LeylineV6, "node_defs", true},
		{"projection-v4 _ast", fixturedb.Leyline, "_ast", true},
		{"standalone node_defs", fixturedb.Standalone, "node_defs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := paritySpec(t, tc.producer).DB()
			expr, ok, err := NodeHashExpr(DB(db), tc.table, "a")
			require.NoError(t, err)
			assert.Equal(t, tc.hashed, ok)
			var hashes [][]byte
			rows, err := db.Query(`SELECT ` + expr + ` FROM ` + tc.table + ` a`)
			require.NoError(t, err)
			for rows.Next() {
				var h []byte
				require.NoError(t, rows.Scan(&h))
				hashes = append(hashes, h)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close()) // the fixture db has one connection
			n := len(hashes)
			for _, h := range hashes {
				if !tc.hashed {
					assert.Nil(t, h)
					continue
				}
				require.Len(t, h, 32, "a merkle node_hash")
				assert.True(t, inNodeContent(t, db, h), "the hash is a node_content identity")
			}
			assert.Positive(t, n)
		})
	}
}

func inNodeContent(t *testing.T, db *sql.DB, h []byte) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM node_content WHERE node_hash = ?`, h).Scan(&n))
	return n == 1
}
