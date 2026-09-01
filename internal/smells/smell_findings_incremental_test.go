package smells

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
// functions, plus empty node_defs/node_refs so RunSmellRule's
// EnsureCanonicalViews probe has real tables to read. It enforces the
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

// TestCyclomaticMemo_KindsMatchRule is the drift guard across all three copies
// of the counted-kind set: the rule SQL (cyclomatic_complexity.json), the
// PRODUCTION const cyclomaticBranchKinds (the list that actually computes the
// memoized metric), and the test's countedKinds (what the fixtures exercise).
// The production const is the load-bearing one — a memo that counts a different
// kind set than the rule diverges — so it is checked against the rule directly,
// not just incidentally via the oracle.
func TestCyclomaticMemo_KindsMatchRule(t *testing.T) {
	q := cyclomaticRule(t).Query
	for _, k := range countedKinds {
		assert.Containsf(t, q, "'"+k+"'", "rule no longer counts %q — update countedKinds", k)
	}
	for _, k := range uncountedKinds {
		assert.NotContainsf(t, q, "'"+k+"'", "rule now counts %q — it must move to countedKinds", k)
	}
	// The production kind set must match the rule too — this is the copy that
	// computes the metric, so its drift is a real byte-identity bug.
	for _, k := range cyclomaticBranchKinds {
		assert.Containsf(t, q, "'"+k+"'", "rule no longer counts %q — update cyclomaticBranchKinds", k)
	}
	assert.ElementsMatch(t, countedKinds, cyclomaticBranchKinds,
		"test countedKinds and production cyclomaticBranchKinds must stay in sync")
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

	want, err := RunSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
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

		want, err := RunSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
		require.NoErrorf(t, err, "full scan failed at step %d", step)

		before := memo.hits
		got, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo)
		require.NoErrorf(t, err, "memoized scan failed at step %d", step)
		if memo.hits > before {
			sawHit = true
		}

		assert.Equalf(t, jsonOrPanic(want), jsonOrPanic(got),
			"memoized diverged from full scan at step %d", step)
		// db is closed by buildASTFixture's t.Cleanup — no explicit close here.
	}
	assert.True(t, sawHit, "the persistent memo must serve at least one cache hit across the edit sequence")
}

// TestCyclomaticMemo_LimitTruncation exercises the out[:limit] path (the main
// oracle uses limit=100000 and never truncates). Includes a tie group that
// straddles the limit boundary, stressing the stable-sort tie-break
// (metric DESC, source_id, start_byte) against SQL's ORDER BY..LIMIT.
func TestCyclomaticMemo_LimitTruncation(t *testing.T) {
	var funcs []fixFunc
	// Four functions with metric 2 (same shape, distinct hashes+locations) that
	// straddle a limit cut, plus a couple of distinct metrics above/below.
	tie := []string{"if_statement", "for_statement"} // metric 2
	for i := range 4 {
		funcs = append(funcs, fixFunc{
			sourceID:  fmt.Sprintf("pkg/tie%d.go", i),
			nodeID:    fmt.Sprintf("pkg/tie%d.go/T", i),
			hash:      hashFor(fmt.Sprintf("tie-%d", i)),
			branches:  tie,
			startByte: i * 500,
			startRow:  i * 20,
		})
	}
	funcs = append(funcs,
		fixFunc{sourceID: "pkg/hi.go", nodeID: "pkg/hi.go/H", hash: hashFor("hi"), branches: []string{"if_statement", "for_statement", "case_clause", "type_case"}, startByte: 9000, startRow: 900},
		fixFunc{sourceID: "pkg/lo.go", nodeID: "pkg/lo.go/L", hash: hashFor("lo"), branches: []string{"if_statement"}, startByte: 9500, startRow: 950},
	)

	db := buildASTFixture(t, funcs)
	qg := &sqlDBQuerier{db: db}

	for _, limit := range []int{1, 3, 5} {
		want, err := RunSmellRule(qg, cyclomaticRule(t), "", limit)
		require.NoError(t, err)
		got, err := runCyclomaticComplexityMemo(qg, "", limit, newSmellMemo())
		require.NoError(t, err)
		assert.Equalf(t, jsonOrPanic(want), jsonOrPanic(got),
			"memoized diverged from full scan at limit=%d", limit)
		assert.LessOrEqualf(t, len(got), limit, "limit=%d must truncate", limit)
	}
}

// TestCyclomaticMemo_HashlessAndMixed exercises the f.hashHex=="" path: some
// functions carry no node_hash (nil), mixed with hashed ones. Hashless
// functions must be computed every scan (never cached) and parity must hold
// across a re-run of the persistent memo.
func TestCyclomaticMemo_HashlessAndMixed(t *testing.T) {
	funcs := []fixFunc{
		{sourceID: "pkg/a.go", nodeID: "pkg/a.go/Hashed", hash: hashFor("h1"), branches: []string{"if_statement", "for_statement"}, startByte: 0, startRow: 0},
		{sourceID: "pkg/a.go", nodeID: "pkg/a.go/Nil1", hash: nil, branches: []string{"if_statement", "case_clause", "block"}, startByte: 1000, startRow: 50},
		{sourceID: "pkg/b.go", nodeID: "pkg/b.go/Nil2", hash: nil, branches: []string{"for_statement"}, startByte: 2000, startRow: 100},
		{sourceID: "pkg/b.go", nodeID: "pkg/b.go/HashedDup", hash: hashFor("h1"), branches: []string{"if_statement", "for_statement"}, startByte: 3000, startRow: 150},
	}
	db := buildASTFixture(t, funcs)
	qg := &sqlDBQuerier{db: db}
	want, err := RunSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
	require.NoError(t, err)

	memo := newSmellMemo()
	for run := range 2 {
		got, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo)
		require.NoError(t, err)
		assert.Equalf(t, jsonOrPanic(want), jsonOrPanic(got), "mixed-hash parity failed on run %d", run)
	}
	// The two hashless functions are computed on every scan (never cached): 2
	// per run × 2 runs = 4 hashless computes. The shared h1 subtree is computed
	// once and cache-hit thereafter.
	assert.GreaterOrEqual(t, memo.misses, 4, "each hashless occurrence must be (re)computed every scan")
}

// TestCyclomaticMemo_SourceScoped exercises scanASTFunctions' sourceID!="" WHERE
// clause: a multi-source fixture scanned scoped to one source must match the
// scoped full scan byte-for-byte.
func TestCyclomaticMemo_SourceScoped(t *testing.T) {
	funcs := []fixFunc{
		{sourceID: "pkg/a.go", nodeID: "pkg/a.go/A1", hash: hashFor("a1"), branches: []string{"if_statement", "for_statement"}, startByte: 0, startRow: 0},
		{sourceID: "pkg/a.go", nodeID: "pkg/a.go/A2", hash: hashFor("a2"), branches: []string{"case_clause"}, startByte: 500, startRow: 25},
		{sourceID: "pkg/b.go", nodeID: "pkg/b.go/B1", hash: hashFor("b1"), branches: []string{"if_statement", "if_statement", "for_statement"}, startByte: 0, startRow: 0},
	}
	db := buildASTFixture(t, funcs)
	qg := &sqlDBQuerier{db: db}

	want, err := RunSmellRule(qg, cyclomaticRule(t), "pkg/a.go", oracleLimit)
	require.NoError(t, err)
	got, err := runCyclomaticComplexityMemo(qg, "pkg/a.go", oracleLimit, newSmellMemo())
	require.NoError(t, err)
	assert.Equal(t, jsonOrPanic(want), jsonOrPanic(got), "source-scoped scan must match")
	for _, f := range got {
		assert.Equal(t, "pkg/a.go", f.SourceID, "scoped scan must not leak other sources")
	}
}

// TestCyclomaticMemo_NoHashColumn exercises scanASTFunctions' hasHash==false
// branch: an _ast with NO node_hash column at all (a standalone / non-merkle
// producer). Every function is computed (nothing cacheable) and parity holds.
func TestCyclomaticMemo_NoHashColumn(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "nohash.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER, end_row INTEGER, end_col INTEGER
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO _ast VALUES ('s.go/F', 's.go', 'function_declaration', 0, 500, 0, 0, 20, 0);
		INSERT INTO _ast VALUES ('s.go/F/if', 's.go', 'if_statement', 10, 20, 1, 0, 0, 0);
		INSERT INTO _ast VALUES ('s.go/F/for', 's.go', 'for_statement', 30, 40, 2, 0, 0, 0);
		INSERT INTO _ast VALUES ('s.go/G', 's.go', 'method_declaration', 600, 900, 30, 0, 45, 0);
	`)
	require.NoError(t, err)
	qg := &sqlDBQuerier{db: db}

	want, err := RunSmellRule(qg, cyclomaticRule(t), "", oracleLimit)
	require.NoError(t, err)
	memo := newSmellMemo()
	got, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo)
	require.NoError(t, err)
	assert.Equal(t, jsonOrPanic(want), jsonOrPanic(got), "no-node_hash-column parity must hold")
	assert.Equal(t, 0, memo.hits, "no node_hash column ⟹ nothing cacheable ⟹ zero cache hits")
}
