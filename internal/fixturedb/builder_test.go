package fixturedb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRef_NodeIDMeansDifferentThingsPerProducer is the property this package
// exists for. `node_refs.node_id` has TWO incompatible meanings, and before
// fixturedb a test picked one implicitly, by what it INSERTed, with nothing in
// the test saying which. Now the meaning is two typed parameters and the
// producer decides where each lands.
func TestRef_NodeIDMeansDifferentThingsPerProducer(t *testing.T) {
	const (
		from = ConstructID("pkg/served_test.go/function_declaration_0")
		at   = SiteID("pkg/served_test.go/function_declaration_0/block/call_expression")
	)

	t.Run("leyline puts the SITE in node_id and the CONSTRUCT in container", func(t *testing.T) {
		b := New(t, Leyline)
		b.Construct(from)
		b.Ref("Served", from, at, "")
		_, f := b.Build()

		var nodeID, container, source string
		require.NoError(t, f.DB().QueryRow(
			`SELECT node_id, container_node_id, source_id FROM node_refs WHERE token='Served'`,
		).Scan(&nodeID, &container, &source))

		assert.Equal(t, string(at), nodeID)
		assert.Equal(t, string(from), container)
		assert.Equal(t, "pkg/served_test.go", source, "source_id inferred from the construct id")
	})

	t.Run("standalone puts the CONSTRUCT in node_id and has nowhere for the site", func(t *testing.T) {
		b := New(t, Standalone)
		b.Construct(from)
		b.Ref("Served", from, at, "")
		_, f := b.Build()

		var nodeID string
		require.NoError(t, f.DB().QueryRow(
			`SELECT node_id FROM node_refs WHERE token='Served'`).Scan(&nodeID))
		assert.Equal(t, string(from), nodeID)

		assert.False(t, hasColumn(t, f.DB(), "node_refs", "container_node_id"),
			"the mache projection has no container column at all")
	})
}

// TestRef_ProducerSelectsTheReferrerArm walks the consequence all the way to the
// SQL: ensureCanonicalViews emits a COALESCE over container_node_id
// when the column exists and a bare `node_id` when it does not, so the two
// producers make v_refs.referrer_node_id resolve to different things from the
// SAME test source. That divergence used to be selected by a fixture's CREATE
// TABLE literal.
func TestRef_ProducerSelectsTheReferrerArm(t *testing.T) {
	const (
		from = ConstructID("pkg/run.go/function_declaration_0")
		at   = SiteID("pkg/run.go/function_declaration_0/block/call_expression")
	)

	// Without an installed view this asserts on the raw shape, which is the
	// input ensureCanonicalViews probes. The end-to-end assertion lives in
	// cmd, where the real installer is registered.
	for _, tc := range []struct {
		producer     Producer
		wantReferrer string
	}{
		{Leyline, string(from)},
		{Standalone, string(from)},
	} {
		t.Run(tc.producer.String(), func(t *testing.T) {
			b := New(t, tc.producer)
			b.Ref("Orphan", from, at, "")
			_, f := b.Build()

			var referrer string
			q := `SELECT COALESCE(NULLIF(container_node_id,''), node_id) FROM node_refs`
			if !hasColumn(t, f.DB(), "node_refs", "container_node_id") {
				q = `SELECT node_id FROM node_refs`
			}
			require.NoError(t, f.DB().QueryRow(q).Scan(&referrer))
			assert.Equal(t, tc.wantReferrer, referrer,
				"both producers must agree on the CALLER; only the column they store it in differs")
		})
	}
}

// TestRef_EmptySiteSynthesisesDistinctLeaves pins that two unnamed occurrences
// in one construct are two rows on ley-line, not one. Collapsing them is the
// fan_out_skew miscount (mache-50e939).
func TestRef_EmptySiteSynthesisesDistinctLeaves(t *testing.T) {
	b := New(t, Leyline)
	b.Ref("helper", "a.go/function_declaration_0", "", "")
	b.Ref("helper", "a.go/function_declaration_0", "", "")
	_, f := b.Build()

	rows, err := f.DB().Query(`SELECT DISTINCT node_id FROM node_refs WHERE token='helper'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	assert.Len(t, ids, 2, "two call sites are two leaves")
}

// TestRef_EmptyQualifierIsNULL matches ley-line exactly: a bare call stores NULL,
// not the empty string. ensureCanonicalViews wraps it in a COALESCE to the
// empty string, so a fixture that stored one directly never exercised it.
func TestRef_EmptyQualifierIsNULL(t *testing.T) {
	b := New(t, Leyline)
	b.Ref("Alpha", "a.go/function_declaration_1", "", "")
	b.Ref("Println", "a.go/function_declaration_0", "", "fmt")
	_, f := b.Build()

	var bare sql.NullString
	require.NoError(t, f.DB().QueryRow(
		`SELECT qualifier FROM node_refs WHERE token='Alpha'`).Scan(&bare))
	assert.False(t, bare.Valid, "ley-line writes NULL for an unqualified call")

	var qualified sql.NullString
	require.NoError(t, f.DB().QueryRow(
		`SELECT qualifier FROM node_refs WHERE token='Println'`).Scan(&qualified))
	assert.Equal(t, "fmt", qualified.String)
}

// TestDef_CanonicalKindOnlyExistsOnLeyline pins that the κ vocabulary is a
// ley-line column. A Standalone fixture cannot carry it, so a rule that filters
// on canonical_kind must degrade there rather than appear to work.
func TestDef_CanonicalKindOnlyExistsOnLeyline(t *testing.T) {
	bl := New(t, Leyline)
	bl.Def("Handle", "pkg/recv.go/method_declaration_0", Method)
	_, fl := bl.Build()

	var kind string
	require.NoError(t, fl.DB().QueryRow(
		`SELECT canonical_kind FROM node_defs WHERE token='Handle'`).Scan(&kind))
	assert.Equal(t, "method", kind)

	bs := New(t, Standalone)
	bs.Def("Handle", "pkg/recv.go/method_declaration_0", Method)
	_, fs := bs.Build()
	assert.False(t, hasColumn(t, fs.DB(), "node_defs", "canonical_kind"))
}

// TestDetailSubtree_SharesOneNodeHash pins the merkle one-to-many invariant
// without any test ever writing hash bytes: occurrences labelled the same share
// a node_hash, differently-labelled ones do not.
func TestDetailSubtree_SharesOneNodeHash(t *testing.T) {
	b := New(t, Leyline)
	b.Def("dup", "a.go/fn_0", Function, Detail{Subtree: "shared"})
	b.Def("dup", "b.go/fn_0", Function, Detail{Subtree: "shared"})
	b.Def("solo", "c.go/fn_0", Function)
	_, f := b.Build()

	var shared int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(DISTINCT node_hash) FROM node_defs WHERE token='dup'`).Scan(&shared))
	assert.Equal(t, 1, shared, "one deduped subtree behind two occurrences")

	var occurrences int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM node_defs WHERE token='dup'`).Scan(&occurrences))
	assert.Equal(t, 2, occurrences, "…and the occurrences must not collapse")

	var overlap int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM node_defs WHERE token='solo'
		   AND node_hash IN (SELECT node_hash FROM node_defs WHERE token='dup')`).Scan(&overlap))
	assert.Zero(t, overlap, "unrelated subtrees must not collide")
}

// TestNew_RejectsTheZeroProducer is the constructor's half of the closed set:
// Producer's only field is unexported, so no caller can build a third value, and
// the zero value is refused here.
func TestNew_RejectsTheZeroProducer(t *testing.T) {
	assert.False(t, Producer{}.valid())
	assert.Equal(t, "invalid(zero-value Producer)", Producer{}.String())
	assert.True(t, Leyline.valid())
	assert.True(t, Standalone.valid())
}

// TestInferSource pins the id→source_id inference, which is how ley-line
// composes node ids ("<source_id>/<tree-sitter path>").
func TestInferSource(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"pkg/orphan.go/function_declaration_0", "pkg/orphan.go"},
		{"src/lib.rs/function_item_0", "src/lib.rs"},
		{"internal/x/testdata/deep.go/function_declaration_0", "internal/x/testdata/deep.go"},
		{"a.go", "a.go"},
		{"pkg/functions/Run", ""}, // mache-schema path: no file segment
	} {
		assert.Equal(t, SourceID(tc.want), inferSource(ConstructID(tc.id)), tc.id)
	}
}

// TestSource_ContentIsNULLWhenUnset matches ley-line v0.13.0, which stores bytes
// in source_blobs and leaves _source.content NULL — the assumption
// smell_test_nodes.go's structural detection is built on.
func TestSource_ContentIsNULLWhenUnset(t *testing.T) {
	b := New(t, Leyline)
	b.Source("a.go", "go", "")
	b.Source("b.go", "go", "package b\n")
	_, f := b.Build()

	var empty sql.RawBytes
	var got sql.NullString
	_ = empty
	require.NoError(t, f.DB().QueryRow(`SELECT content FROM _source WHERE id='a.go'`).Scan(&got))
	assert.False(t, got.Valid, "ley-line leaves _source.content NULL")

	require.NoError(t, f.DB().QueryRow(`SELECT content FROM _source WHERE id='b.go'`).Scan(&got))
	assert.Equal(t, "package b\n", got.String)
}

// TestBuild_SingleConnection pins the TEMP-object requirement centrally: TEMP
// views are per-connection, so a fixture that let the pool hand out a second
// connection would lose v_defs / v_refs non-deterministically. Before fixturedb
// each fixture decided this for itself, and most did not.
func TestBuild_SingleConnection(t *testing.T) {
	b := New(t, Leyline)
	b.Def("Run", "a.go/fn_0", Function)
	_, f := b.Build()

	_, err := f.DB().Exec(`CREATE TEMP VIEW probe AS SELECT 1 AS x`)
	require.NoError(t, err)
	for range 8 {
		var x int
		require.NoError(t, f.DB().QueryRow(`SELECT x FROM probe`).Scan(&x),
			"the TEMP view must survive every query — one connection only")
	}
}

func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		if n == col {
			return true
		}
	}
	require.NoError(t, rows.Err())
	return false
}
