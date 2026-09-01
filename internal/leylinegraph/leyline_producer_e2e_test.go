//go:build unix

package leylinegraph

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMaterializeVirtuals_AgainstRealProducerArena runs the write path against
// an arena a REAL leyline produced, rather than against a fixture mache shaped
// itself.
//
// This is the gate that answers "does mache work with that leyline?" without
// waiting for a release. By default it runs against the cached pin, so it is a
// standing end-to-end check on the shipped projection. Point leyline.BinaryOverrideEnv
// (MACHE_LEYLINE_BINARY) at a release candidate and the SAME assertions run
// against the candidate's projection — which is the whole point of mache-cc1a70:
// the previous alternative was to release, discover drift, and fix.
//
// The assertions are deliberately shape-independent. `mache serve` reaches this
// code through openDBGraph on whatever .db it is given, so what must hold is
// that the virtual nodes exist and are correctly PARENTED — the property that
// breaks silently when parent_id becomes a generated column (mache-bc6ca3),
// because a wrong parent surfaces as an empty directory, never as an error.
func TestMaterializeVirtuals_AgainstRealProducerArena(t *testing.T) {
	ll := lltest.ResolveBinaryOrSkip(t)

	dir, err := os.MkdirTemp("/tmp", "llprod")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	// Beta calls Alpha, so node_refs/node_defs carry a resolvable pair and the
	// callers/ and callees/ materializers have something to project.
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"), []byte(
		"package a\n\nimport \"fmt\"\n\nfunc Alpha() { fmt.Println(\"hi\") }\n\nfunc Beta() { Alpha() }\n"), 0o644))

	dbPath := filepath.Join(dir, "out.db")
	if out, perr := exec.Command(ll.Path, "parse", src, "-o", dbPath).CombinedOutput(); perr != nil {
		t.Fatalf("leyline parse failed (%s): %v\n%s", ll.Path, perr, out)
	}

	// Record the shape this producer actually emitted, so a failure says which
	// projection it was against rather than leaving that to be guessed.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var projection string
	if err := db.QueryRow(
		`SELECT value FROM _meta WHERE key = 'projection_schema_version'`).Scan(&projection); err != nil {
		projection = "(absent — arena predates the _meta channel)"
	}
	var hidden int
	if err := db.QueryRow(
		`SELECT hidden FROM pragma_table_xinfo('nodes') WHERE name='parent_id'`).Scan(&hidden); err != nil {
		t.Fatalf("producer arena has no nodes.parent_id at all: %v", err)
	}
	t.Logf("producer %s: projection=%s, parent_id hidden=%d (0 stored, 2 GENERATED VIRTUAL)",
		ll.Version, projection, hidden)

	schema := &api.Topology{
		Version: "v1",
		Nodes:   []api.Node{{Name: "functions", Selector: "(function_declaration)"}},
	}
	require.NoError(t, MaterializeVirtuals(dbPath, schema, true),
		"MaterializeVirtuals must succeed against a real %s arena (projection %s)",
		ll.Version, projection)

	// _schema.json is a root, so its derived parent is '' either way.
	var parent, name string
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(parent_id,''), name FROM nodes WHERE id = '_schema.json'`).Scan(&parent, &name))
	assert.Equal(t, "", parent, "a root virtual node must have the empty parent")
	assert.Equal(t, "_schema.json", name)

	// The load-bearing case: a NESTED virtual node. Under a derived parent_id
	// this is where a wrong id/name pairing would silently reparent the row to
	// the root and empty out the directory.
	rows, err := db.Query(
		`SELECT id, COALESCE(parent_id,''), name FROM nodes WHERE name = 'callers'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var id, p, n string
		require.NoError(t, rows.Scan(&id, &p, &n))
		seen++
		assert.Equal(t, p+"/"+n, id,
			"callers/ dir %q must sit under the construct its id names; a mismatch here is "+
				"an orphaned directory, which reads as empty rather than failing", id)
	}
	require.NoError(t, rows.Err())
	require.NotZero(t, seen,
		"no callers/ dirs were materialized — the corpus has a resolvable call, so this "+
			"means the producer's node_defs/node_refs shape changed under us")
}
