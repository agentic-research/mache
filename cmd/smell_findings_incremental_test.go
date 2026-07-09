package cmd

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// mache-238673 phase-1 — the DIFFERENTIAL ORACLE for node_hash-memoized
// cyclomatic_complexity. THE close condition (per the acceptance amendment):
// `memoized(db) == full_scan(db)` BYTE-IDENTICAL findings, so a memoization /
// invalidation bug can only FAIL the test, never pass silently.
//
// Why byte-identical is sufficient: cyclomatic_complexity is a PURE-SUBTREE
// rule — the metric is the count of control-flow descendants of a function, so
// it is fully determined by the function subtree content = its merkle
// node_hash. Same hash ⟹ same subtree ⟹ same metric, SOUND BY CONSTRUCTION.
// The memo caches ONLY the metric (an int64) keyed by node_hash and re-reads
// every occurrence's span fresh, so a duplicated subtree is computed once and
// a moved-but-unchanged function keeps its new span. Any divergence from the
// full scan — stale metric, stale span, dropped occurrence, wrong order — is a
// bug the oracle catches.

// counted / uncounted are the AST node_kinds the fixture draws branches from.
// counted MUST stay in sync with cmd/rules/cyclomatic_complexity.json (the
// oracle catches drift: a memo that counts a different kind set diverges from
// the rule's own SQL). uncounted kinds appear in the subtree — and thus in the
// node_hash — but must NOT contribute to the metric, pinning that the memo
// counts the same subset the rule does.
var (
	countedKinds = []string{
		"if_statement", "for_statement", "case_clause",
		"expression_case", "type_case", "communication_case", "default_case",
	}
	uncountedKinds = []string{
		"block", "return_statement", "expression_statement", "call_expression",
	}
)

// fixFunc is one function occurrence in a synthetic _ast fixture. branches is
// the ordered list of child node_kinds (both counted and uncounted); it is the
// function's "content". INVARIANT (enforced by buildASTFixture): two fixFuncs
// with the same hash MUST have identical branches — that is what makes the
// fixture an honest model of content-addressing. Two occurrences that share a
// hash are byte-identical subtrees (a deduped merkle node); occurrences with
// distinct hashes are distinct content.
type fixFunc struct {
	sourceID  string
	nodeID    string
	hash      []byte // merkle node_hash of the subtree; nil ⟹ no node_hash (standalone db)
	branches  []string
	startByte int
	startRow  int
}

// hashFor derives a stable node_hash from a content string. Distinct content
// strings ⟹ distinct hashes; identical content ⟹ identical hash — the
// content-addressing invariant the producer guarantees.
func hashFor(content string) []byte {
	h := sha256.Sum256([]byte(content))
	return h[:16]
}

// buildASTFixture writes an _ast db (with a node_hash column) for the given
// functions, plus empty node_defs/node_refs so runSmellRule's
// ensureCanonicalViews probe has real tables to read. It enforces the
// content-addressing invariant: same hash ⟹ same branches.
func buildASTFixture(t testing.TB, funcs []fixFunc) *sql.DB {
	t.Helper()

	// Guard: same hash ⟹ same branch shape, else the fixture lies about
	// content-addressing and the oracle would be meaningless.
	byHash := map[string]string{}
	for _, f := range funcs {
		if f.hash == nil {
			continue
		}
		key := fmt.Sprintf("%x", f.hash)
		shape := strings.Join(f.branches, ",")
		if prev, ok := byHash[key]; ok {
			require.Equalf(t, prev, shape,
				"fixture invariant violated: node_hash %s maps to two different branch shapes", key)
		}
		byHash[key] = shape
	}

	dbPath := filepath.Join(t.TempDir(), "incr.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER,
			node_hash BLOB
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
	`)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	for _, f := range funcs {
		_, err := tx.Exec(
			`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			 VALUES (?, ?, 'function_declaration', ?, ?, ?, 0, ?, 0, ?)`,
			f.nodeID, f.sourceID, f.startByte, f.startByte+500, f.startRow, f.startRow+20, f.hash,
		)
		require.NoError(t, err)
		for j, kind := range f.branches {
			_, err := tx.Exec(
				`INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
				 VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0)`,
				fmt.Sprintf("%s/branch_%d", f.nodeID, j), f.sourceID, kind,
				f.startByte+10+j, f.startByte+11+j, f.startRow+1+j,
			)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tx.Commit())
	return db
}

// cyclomaticRule returns the embedded cyclomatic_complexity rule.
func cyclomaticRule(t testing.TB) *SmellRule {
	t.Helper()
	for _, r := range mustLoadEmbeddedRules() {
		if r.ID == "cyclomatic_complexity" {
			return &r
		}
	}
	t.Fatal("cyclomatic_complexity rule not found in embedded ruleset")
	return nil
}

const oracleLimit = 100000

// TestCyclomaticMemo_KindsMatchRule pins that the fixture's countedKinds are
// exactly the kinds the rule's SQL counts — a drift guard so the oracle can't
// silently stop exercising a kind the rule cares about.
func TestCyclomaticMemo_KindsMatchRule(t *testing.T) {
	q := cyclomaticRule(t).Query
	for _, k := range countedKinds {
		assert.Containsf(t, q, "'"+k+"'", "rule no longer counts %q — update countedKinds", k)
	}
	for _, k := range uncountedKinds {
		assert.NotContainsf(t, q, "'"+k+"'", "rule now counts %q — it must move to countedKinds", k)
	}
}

// TestCyclomaticMemo_DuplicatedSubtrees is oracle case (b): a subtree that
// occurs N times shares ONE node_hash. The memoized scan must (1) surface all
// N occurrences byte-identically to the full scan, and (2) compute the metric
// exactly ONCE for the group (misses==1, hits==N-1) — proving dedup, not just
// correctness.
func TestCyclomaticMemo_DuplicatedSubtrees(t *testing.T) {
	shape := []string{"if_statement", "for_statement", "block", "case_clause"} // metric 3
	dupHash := hashFor("dup-subtree")
	var funcs []fixFunc
	const n = 5
	for i := range n {
		funcs = append(funcs, fixFunc{
			sourceID:  fmt.Sprintf("pkg/f%d.go", i),
			nodeID:    fmt.Sprintf("pkg/f%d.go/Dup", i),
			hash:      dupHash,
			branches:  shape,
			startByte: i * 1000,
			startRow:  i * 40,
		})
	}
	// One distinct function so the scan isn't a single group.
	funcs = append(funcs, fixFunc{
		sourceID: "pkg/other.go", nodeID: "pkg/other.go/Solo",
		hash: hashFor("solo"), branches: []string{"if_statement"}, startByte: 99000, startRow: 5000,
	})

	db := buildASTFixture(t, funcs)
	qg := &sqlDBQuerier{db: db}

	want, err := runSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
	require.NoError(t, err)

	memo := newSmellMemo()
	got, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo)
	require.NoError(t, err)

	assert.Equal(t, jsonOrPanic(want), jsonOrPanic(got), "memoized findings must be byte-identical to the full scan")
	assert.Len(t, got, n+1, "all N duplicated occurrences plus the solo must surface")
	assert.Equal(t, 2, memo.misses, "exactly two distinct node_hashes computed (dup group + solo)")
	assert.Equal(t, n-1, memo.hits, "the remaining occurrences of the dup group must be cache hits")
}

// TestCyclomaticMemo_DifferentialOracle_RandomEdits is oracle case (a): a
// persistent memo carried across a random edit sequence must ALWAYS agree with
// a fresh full scan of the current db. Edits include add, delete, mutate
// (content change ⟹ new hash ⟹ recompute) and move (span change, same hash ⟹
// cache hit but the NEW span must surface). A stale metric, stale span, or
// dropped occurrence diverges and fails the oracle.
func TestCyclomaticMemo_DifferentialOracle_RandomEdits(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE))
	randBranches := func() []string {
		n := rng.Intn(6)
		out := make([]string, 0, n)
		for range n {
			pool := countedKinds
			if rng.Intn(3) == 0 {
				pool = uncountedKinds
			}
			out = append(out, pool[rng.Intn(len(pool))])
		}
		return out
	}
	newFunc := func(id int) fixFunc {
		br := randBranches()
		// hash keyed on a unique id + content so distinct funcs get distinct
		// hashes; a later mutate rekeys the hash from the new content.
		return fixFunc{
			sourceID:  fmt.Sprintf("pkg/s%d.go", id%7),
			nodeID:    fmt.Sprintf("pkg/s%d.go/F%d", id%7, id),
			hash:      hashFor(fmt.Sprintf("f%d:%s", id, strings.Join(br, ","))),
			branches:  br,
			startByte: id * 777,
			startRow:  id * 13,
		}
	}

	funcs := []fixFunc{}
	nextID := 0
	for range 6 {
		funcs = append(funcs, newFunc(nextID))
		nextID++
	}

	memo := newSmellMemo()
	sawHit := false
	for step := range 60 {
		switch rng.Intn(4) {
		case 0: // add
			funcs = append(funcs, newFunc(nextID))
			nextID++
		case 1: // delete
			if len(funcs) > 0 {
				i := rng.Intn(len(funcs))
				funcs = append(funcs[:i], funcs[i+1:]...)
			}
		case 2: // mutate content: new branches ⟹ new hash (content-addressing)
			if len(funcs) > 0 {
				i := rng.Intn(len(funcs))
				br := randBranches()
				funcs[i].branches = br
				funcs[i].hash = hashFor(fmt.Sprintf("f%s:%s", funcs[i].nodeID, strings.Join(br, ",")))
			}
		case 3: // move: same content/hash, new span — metric reused, span must refresh
			if len(funcs) > 0 {
				i := rng.Intn(len(funcs))
				funcs[i].startByte += 100000 + step
				funcs[i].startRow += 1000 + step
			}
		}

		db := buildASTFixture(t, funcs)
		qg := &sqlDBQuerier{db: db}

		want, err := runSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
		require.NoErrorf(t, err, "full scan failed at step %d", step)

		before := memo.hits
		got, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo)
		require.NoErrorf(t, err, "memoized scan failed at step %d", step)
		if memo.hits > before {
			sawHit = true
		}

		assert.Equalf(t, jsonOrPanic(want), jsonOrPanic(got),
			"memoized diverged from full scan at step %d", step)
		_ = db.Close()
	}
	assert.True(t, sawHit, "the persistent memo must serve at least one cache hit across the edit sequence")
}
