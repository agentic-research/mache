package testfixtures

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update regenerates the goldens. It ALWAYS prints the added/removed rows so
// a regeneration is a reviewed change, never a silent re-fit:
//
//	go test ./internal/testfixtures -run TestGoldenProjection -update
var update = flag.Bool("update", false, "rewrite testdata/golden/projection/*.tsv and print what changed")

// goldenFixtures are the fixtures whose projection is pinned byte-for-byte.
// Only hand-written corpora belong here — mache-self changes every commit.
var goldenFixtures = []string{"small-go-golden"}

func goldenPath(t *testing.T, id string) string {
	t.Helper()
	root, err := findRepoRoot()
	require.NoError(t, err)
	return filepath.Join(root, "testdata", "golden", "projection", id+".tsv")
}

// TestGoldenProjection is the projection-parity gate (mache-c0537f gate 1):
// the projection of each golden fixture must be byte-identical to its
// committed dump. Anything that changes a node id, a ref, a def, a construct's
// bytes or its props trips it — a dedup-suffix reorder, a walker query
// rewrite, a ley-line pin bump that changes what `_ast` contains. The
// failure lists the rows that vanished and the rows that appeared; if the
// change is intended, regenerate with -update and commit the new golden in
// the same PR as the code that changed it.
func TestGoldenProjection(t *testing.T) {
	for _, id := range goldenFixtures {
		t.Run(id, func(t *testing.T) {
			g := Get(t, id)
			srcPath, err := ResolvePath(id)
			require.NoError(t, err)

			got, err := DumpProjection(g.DB(), srcPath)
			require.NoError(t, err)
			require.NotContains(t, got, srcPath,
				"fixture path leaked into the dump; DumpProjection's normalization missed a column")

			path := goldenPath(t, id)
			if *update {
				old, _ := os.ReadFile(path) // absent on first generation
				removed, added := diffLines(string(old), got)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				t.Logf("rewrote %s: %d rows removed, %d rows added", path, len(removed), len(added))
				for _, l := range removed {
					t.Logf("  - %s", l)
				}
				for _, l := range added {
					t.Logf("  + %s", l)
				}
				return
			}

			want, err := os.ReadFile(path)
			require.NoError(t, err, "no golden for %s — generate it with -update", id)
			if string(want) == got {
				return
			}
			removed, added := diffLines(string(want), got)
			var msg strings.Builder
			msg.WriteString("projection of " + id + " drifted from " + path + "\n")
			for _, l := range removed {
				msg.WriteString("  - " + l + "\n")
			}
			for _, l := range added {
				msg.WriteString("  + " + l + "\n")
			}
			msg.WriteString("if this change is intended: go test ./internal/testfixtures -run TestGoldenProjection -update")
			t.Fatal(msg.String())
		})
	}
}

// TestGoldenProjection_CoversEveryWriterTable pins the dump to the writer's
// table set in both directions: a table the SQLiteWriter starts populating
// that the dump does not render would let projection drift through unseen,
// and a section for a table the writer no longer creates is dead.
func TestGoldenProjection_CoversEveryWriterTable(t *testing.T) {
	g := Get(t, goldenFixtures[0])
	rows, err := g.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())

	var dumped []string
	for _, s := range projectionSections {
		dumped = append(dumped, s.name)
	}
	assert.ElementsMatch(t, tables, dumped,
		"tables in a projected .db and sections DumpProjection renders differ")
}

// TestDumpProjection_IsDeterministic: two dumps of the same db are the same
// bytes, so a golden mismatch is a projection difference and never dump
// nondeterminism (map order, unsorted rows).
func TestDumpProjection_IsDeterministic(t *testing.T) {
	id := goldenFixtures[0]
	g := Get(t, id)
	srcPath, err := ResolvePath(id)
	require.NoError(t, err)
	a, err := DumpProjection(g.DB(), srcPath)
	require.NoError(t, err)
	b, err := DumpProjection(g.DB(), srcPath)
	require.NoError(t, err)
	assert.Equal(t, a, b)
	header, _, _ := strings.Cut(a, "\n")
	assert.True(t, strings.HasPrefix(header, "## nodes ("), "dump starts with the nodes section, got %q", header)
}

func TestDiffLines(t *testing.T) {
	removed, added := diffLines("a\nb\nc\n", "b\nc\nd\n")
	assert.Equal(t, []string{"a"}, removed)
	assert.Equal(t, []string{"d"}, added)

	removed, added = diffLines("x\n", "x\n")
	assert.Empty(t, removed)
	assert.Empty(t, added)
}

// TestProjectionInvariant_EveryParentIsANode is the reachability gate: a node
// whose parent_id names no node is written to the db but is invisible to the
// tree — ListChildren is `WHERE parent_id = ?`, so list_directory and a mount
// both skip it while node_defs still lists its token.
//
// It caught the slashed-import defect (mache-94f571): SQLiteWriter.AddNode
// derives parent_id from the LAST '/' of the ID, so a name that renders a '/'
// split in the wrong place — `main/imports/"golden/store"` claimed the parent
// `main/imports/"golden`, which does not exist. 1 orphan on small-go-golden,
// 1939 on mache itself, every one of them an import of a slashed path.
//
// mache-self runs under the same opt-in as the other full-repo fixtures; it
// is the only corpus wide enough to cover every preset shape mache projects.
func TestProjectionInvariant_EveryParentIsANode(t *testing.T) {
	for _, id := range []string{"small-go-golden", "mache-self"} {
		t.Run(id, func(t *testing.T) {
			if id == "mache-self" && os.Getenv("MACHE_E2E_LARGE") == "" {
				t.Skip("full-repo fixture (~20s); set MACHE_E2E_LARGE=1 to run")
			}
			db := Get(t, id).DB()
			const orphans = `SELECT n.id, n.parent_id FROM nodes n
				WHERE n.parent_id IS NOT NULL AND n.parent_id != ''
				  AND NOT EXISTS (SELECT 1 FROM nodes p WHERE p.id = n.parent_id)
				ORDER BY n.id`
			rows, err := db.Query(orphans)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()

			var found []string
			for rows.Next() {
				var nodeID, parentID string
				require.NoError(t, rows.Scan(&nodeID, &parentID))
				found = append(found, nodeID+" -> "+parentID)
			}
			require.NoError(t, rows.Err())
			assert.Empty(t, found,
				"%d node(s) have a parent_id that is not a node id, so ListChildren can never "+
					"return them; a rendered name reached the id with a '/' still in it "+
					"(ingest.segmentEscape)", len(found))
		})
	}
}

// TestProjectionInvariants are the load-bearing intents behind rows in the
// golden (mache-c0537f gate 3): a regeneration that silently dropped one of
// these would still produce a self-consistent golden, so each is pinned by
// name with exact ids, independent of the byte-for-byte diff. They are the
// go-preset counterparts of cmd's FCA-path tests (TestBuild_FCAInferenceCoversMethods,
// TestBuild_FCAInferenceTagsLanguage) — same invariants, the other projection path.
func TestProjectionInvariants(t *testing.T) {
	db := Get(t, "small-go-golden").DB()
	count := func(t *testing.T, q string, args ...any) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRow(q, args...).Scan(&n), q)
		return n
	}

	// A package with receiver methods gets a methods/ root, and BOTH receiver
	// shapes land under it. Dropping either selector from the preset (or the
	// walker silently failing to match one) collapses methods into nothing,
	// which is how every method-only caller became dead_code (mache-5d1o).
	t.Run("methods root holds pointer and value receivers", func(t *testing.T) {
		assert.Equal(t, 1, count(t, `SELECT COUNT(*) FROM nodes WHERE id = 'store/methods' AND kind = 1`))
		assert.Equal(t, 1, count(t, `SELECT COUNT(*) FROM nodes WHERE id = 'store/methods/Store.Add/source'`), "pointer receiver")
		assert.Equal(t, 1, count(t, `SELECT COUNT(*) FROM nodes WHERE id = 'store/methods/Store.Len/source'`), "value receiver")
	})

	// A method's call to a package function is a node_refs row on the
	// method's source node — the cross-ref dead_code needs to see that keyOf
	// is alive even though only a method calls it.
	t.Run("method to callee cross-ref", func(t *testing.T) {
		assert.Equal(t, 1, count(t,
			`SELECT COUNT(*) FROM node_refs WHERE token = 'keyOf' AND node_id = 'store/methods/Store.Add/source'`))
		assert.Equal(t, 1, count(t, `SELECT COUNT(*) FROM node_defs WHERE token = 'keyOf' AND node_id = 'store/functions/keyOf'`))
	})

	// A source file in a language the preset has no nodes for is routed to
	// _project_files/, never matched against the Go selectors: gen.py must be
	// a raw file there and must not have produced a construct anywhere.
	t.Run("non-Go source routed to _project_files", func(t *testing.T) {
		assert.Equal(t, 1, count(t, `SELECT COUNT(*) FROM nodes WHERE id = '_project_files/scripts/gen.py' AND kind = 0`))
		assert.Equal(t, 0, count(t, `SELECT COUNT(*) FROM nodes WHERE name = 'gen' OR id LIKE '%/gen/source'`))
		assert.Equal(t, 0, count(t, `SELECT COUNT(*) FROM file_index WHERE path LIKE '%.py'`),
			"routed files are not in file_index; only projected source files are")
	})

	// Dedup suffixes follow lexical order over the FULL path (engine_treesitter.go
	// ingestSourceTree): tool.go sorts before tool/main.go, so tool.go owns the
	// bare id. WalkDir order would hand it to tool/main.go.
	t.Run("dedup suffix follows lexical full-path order", func(t *testing.T) {
		assert.Equal(t, 1, count(t,
			`SELECT COUNT(*) FROM nodes WHERE id = 'main/functions/init/source' AND source_file LIKE '%/tool.go'`))
		assert.Equal(t, 1, count(t,
			`SELECT COUNT(*) FROM nodes WHERE id = 'main/functions/init.from_main_go/source' AND source_file LIKE '%/tool/main.go'`))
	})
}
