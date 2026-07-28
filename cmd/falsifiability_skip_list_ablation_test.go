package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Falsifiability A — skip-list ablation experiment per ADR-0013
// (mache-3509aa). The headline test of the ADR's claim that LSP
// binding rows make the dead_code skip-list redundant for the cases
// LSP actually sees.
//
// Hypothesis: with _lsp_refs populated, dropping the dead_code
// skip-list flags zero NEW dead constructs that LSP indexed. Any
// new finding must be a method on a type LSP didn't see (genuine
// coverage gap — wedge case 1 from the ADR).
//
// Procedure:
//
//   1. Run dead_code in production form (skip-list intact). Findings = A.
//   2. Run dead_code with the skipped CTE neutralized. Findings = B.
//   3. Diff = B \ A — newly flagged dead.
//   4. For each row in diff, look up its node_id in _lsp_defs and
//      assert NO _lsp_refs row references that def. If LSP HAS a
//      reference but the rule still flags dead, the design is wrong
//      (the alive-check's binding arm — r.target_node_id = d.node_id —
//      isn't kicking in).
//
// Two tests: synthetic harness (always runs) + integration on a
// real LLO-built .db (gated on env + leyline binary).

// stripSkippedCTE rewrites a dead_code rule body to neutralize the
// skipped CTE. Uses paren-depth balancing because the SQL has nested
// parens (the `IN ('a', 'b', ...)` clause) inside the CTE body.
func stripSkippedCTE(query string) (string, error) {
	const marker = "skipped AS ("
	startIdx := strings.Index(query, marker)
	if startIdx < 0 {
		return "", fmt.Errorf("skipped CTE not found in rule body — production SQL may have refactored; update the test")
	}
	// Walk forward from the open paren of `skipped AS (`, balancing
	// depth, to find the matching close paren.
	openIdx := startIdx + len(marker) - 1 // index of '('
	depth := 0
	closeIdx := -1
	for i := openIdx; i < len(query); i++ {
		c := query[i]
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 {
				closeIdx = i
				break
			}
		}
	}
	if closeIdx < 0 {
		return "", fmt.Errorf("skipped CTE close paren not found — production SQL malformed?")
	}
	// Replace [startIdx, closeIdx] (inclusive) with the empty form.
	const empty = "skipped AS (SELECT '' AS node_id WHERE 0)"
	return query[:startIdx] + empty + query[closeIdx+1:], nil
}

func TestStripSkippedCTE_RealRuleBody(t *testing.T) {
	// Find the dead_code rule in the production registry. If a future
	// refactor moves the skip-list out of an inline CTE, this test
	// fails LOUDLY — which is exactly what we want.
	var deadCodeQuery string
	for _, r := range smellRegistry {
		if r.ID == "dead_code" {
			deadCodeQuery = r.Query
			break
		}
	}
	require.NotEmpty(t, deadCodeQuery, "dead_code rule must exist in registry")

	stripped, err := stripSkippedCTE(deadCodeQuery)
	require.NoError(t, err)

	// Original has the long token IN list; stripped has the empty form.
	assert.Contains(t, deadCodeQuery, "'ServeHTTP'", "production rule has the skip-list")
	assert.NotContains(t, stripped, "'ServeHTTP'", "stripped rule no longer has the skip-list")
	assert.Contains(t, stripped, "skipped AS (SELECT '' AS node_id WHERE 0)",
		"stripped rule has the empty CTE substituted")

	// Both must still be valid SQL. Run them against a minimal
	// fixture (no rows) to confirm SQLite parses them.
	dbPath := filepath.Join(t.TempDir(), "syntax.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// TEMP views are per-connection; pin to one connection so the
	// view-creation in ensureCanonicalViews is visible to subsequent
	// db.Query calls in this test. *sql.DB is a pool; without this
	// pin a query might land on a fresh connection without the view.
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
	`)
	require.NoError(t, err)

	// Need v_defs / v_refs for the rule to compile after PR A.
	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// LIMIT 0 — we just want the parser to walk through. Close each
	// rowset immediately; with MaxOpenConns(1) leaving rows open
	// would deadlock the next Query.
	prodRows, err := db.Query(fmt.Sprintf(deadCodeQuery+" LIMIT 0", "" /* scope */))
	require.NoError(t, err, "production dead_code SQL must be valid")
	require.NoError(t, prodRows.Close())
	strippedRows, err := db.Query(fmt.Sprintf(stripped+" LIMIT 0", "" /* scope */))
	require.NoError(t, err, "stripped dead_code SQL must be valid")
	require.NoError(t, strippedRows.Close())
}

// runDeadCodeWithAndWithout runs the dead_code rule against db twice
// — once with the production skip-list, once with it stripped — and
// returns (with-skip findings, without-skip findings). Used by the
// ablation tests to compute the diff.
func runDeadCodeWithAndWithout(t *testing.T, db *sql.DB) ([]string, []string) {
	t.Helper()
	var deadCodeQuery string
	for _, r := range smellRegistry {
		if r.ID == "dead_code" {
			deadCodeQuery = r.Query
			break
		}
	}
	require.NotEmpty(t, deadCodeQuery)

	stripped, err := stripSkippedCTE(deadCodeQuery)
	require.NoError(t, err)

	collect := func(query string) []string {
		final := fmt.Sprintf(query, "") + " LIMIT 100000"
		rows, err := db.Query(final)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var ids []string
		for rows.Next() {
			var (
				src       string
				nodeID    string
				startByte int
				endByte   int
				startRow  int
				startCol  int
				metric    int
			)
			require.NoError(t, rows.Scan(&src, &nodeID, &startByte, &endByte, &startRow, &startCol, &metric))
			ids = append(ids, nodeID)
		}
		return ids
	}
	return collect(deadCodeQuery), collect(stripped)
}

// stringSetDiff returns elements in B that aren't in A.
func stringSetDiff(a, b []string) []string {
	have := map[string]struct{}{}
	for _, s := range a {
		have[s] = struct{}{}
	}
	var diff []string
	for _, s := range b {
		if _, ok := have[s]; !ok {
			diff = append(diff, s)
		}
	}
	return diff
}

func TestFalsifiabilityA_SyntheticHarness(t *testing.T) {
	// Fixture: a method 'Read' on MyType that LSP CAN see (binding
	// row exists). With production skip-list, 'Read' is in skipped →
	// not flagged. With ablation:
	//   - alive-check's binding arm (target_node_id = d.node_id)
	//     finds the LSP ref → 'Read' IS alive → still not flagged.
	// → diff is empty (proves the design works for cases LSP sees).
	//
	// Plus: a method 'Quux' on MyType that LSP does NOT see (no
	// binding row). With production skip-list, 'Quux' is NOT in the
	// hardcoded list and (no qualifier suffix matches Test/Benchmark)
	// → not skip-listed → would be flagged dead in BOTH paths
	// (production and ablation produce the same finding for it).
	dbPath := filepath.Join(t.TempDir(), "ablation.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // pin connection so TEMP views from ensureCanonicalViews persist

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record TEXT, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_defs (
			node_id TEXT NOT NULL, def_token TEXT NOT NULL DEFAULT '',
			def_uri TEXT NOT NULL,
			def_start_line INTEGER NOT NULL, def_start_col INTEGER NOT NULL,
			def_end_line INTEGER NOT NULL, def_end_col INTEGER NOT NULL
		);
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL, referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		-- Construct dirs (kind=1 dir) with source_file fallback rows.
		INSERT INTO nodes VALUES
			('pkg/methods/MyType.Read',  NULL, 'Read',  1, 0, 0, NULL, NULL, NULL),
			('pkg/methods/MyType.Read/source', 'pkg/methods/MyType.Read', 'source', 0, 0, 0, NULL, NULL, 'pkg/mytype.go'),
			('pkg/methods/MyType.Quux',  NULL, 'Quux',  1, 0, 0, NULL, NULL, NULL),
			('pkg/methods/MyType.Quux/source', 'pkg/methods/MyType.Quux', 'source', 0, 0, 0, NULL, NULL, 'pkg/mytype.go');

		-- node_defs: tree-sitter saw both methods.
		INSERT INTO node_defs VALUES
			('Read', 'pkg/methods/MyType.Read'),
			('Quux', 'pkg/methods/MyType.Quux');

		-- node_refs: empty. tree-sitter call-extraction can't resolve
		-- io.Reader.Read() back to MyType, so NO mention rows.
	`)
	require.NoError(t, err)

	// Post-mache-6bd4d8: binding-fidelity rows come from the capnp
	// event log, not the legacy _lsp_refs SQL table. Write a single
	// record for MyType.Read (Quux deliberately omitted — it's the
	// "LSP didn't see this" wedge-case 1 control).
	writeBindingLogForTest(t, dbPath,
		"pkg/methods/MyType.Read", // targetNodeId
		"Read",                    // refToken
		"pkg/functions/Caller",    // constructNodeId (referrer)
		"file:///caller.go")       // refUri

	qg := &sqlDBQuerier{db: db, path: dbPath}
	require.NoError(t, ensureCanonicalViews(qg))
	require.NoError(t, LoadCapnpBindings(qg, qg.DBPath()))

	withSkip, withoutSkip := runDeadCodeWithAndWithout(t, db)

	// 'Read' is in the skip-list → not in withSkip. Ablation drops
	// the skip-list, so the alive-check has to make the call. With
	// the LSP binding row, target_node_id = pkg/methods/MyType.Read
	// matches d.node_id → 'Read' is alive → still not in withoutSkip.
	assert.NotContains(t, withSkip, "pkg/methods/MyType.Read",
		"production skip-list keeps 'Read' off the list")
	assert.NotContains(t, withoutSkip, "pkg/methods/MyType.Read",
		"ablation: LSP binding row keeps 'Read' alive without the skip-list")

	// 'Quux' isn't in the skip-list and has no LSP coverage → flagged
	// dead in BOTH runs.
	assert.Contains(t, withSkip, "pkg/methods/MyType.Quux",
		"'Quux' is not skip-listed and has no refs → dead in production")
	assert.Contains(t, withoutSkip, "pkg/methods/MyType.Quux",
		"'Quux' is not skip-listed and has no refs → dead in ablation too")

	// The headline assertion: ablation MUST NOT flag 'Read' (the LSP-
	// covered case). The diff (withoutSkip \ withSkip) is empty —
	// LSP successfully replaced the skip-list for cases it sees.
	diff := stringSetDiff(withSkip, withoutSkip)
	assert.NotContains(t, diff, "pkg/methods/MyType.Read",
		"Falsifiability A: ablation must not flag 'Read' as dead — LSP saw the call site")
}

func TestFalsifiabilityA_IntegrationOnLeylineParse(t *testing.T) {
	if os.Getenv("MACHE_FALSIFIABILITY_INTEGRATION") != "1" {
		t.Skip("set MACHE_FALSIFIABILITY_INTEGRATION=1 to run; needs leyline + LSP servers indexed")
	}
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH — leyline lsp needs it")
	}

	srcDir, err := os.Getwd()
	require.NoError(t, err)
	repoRoot := filepath.Dir(srcDir)
	dbPath := filepath.Join(t.TempDir(), "self.db")

	// Two-step: parse builds the AST/refs tables, lsp merges in the
	// _lsp_* enrichment. The integration meaningfully exercises the
	// design only when both have run.
	cmd := exec.Command("leyline", "parse", repoRoot, "-o", dbPath, "--lang", "go")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", out)

	enrichTarget := filepath.Join(repoRoot, "validate", "validate.go")
	if _, statErr := os.Stat(enrichTarget); statErr != nil {
		t.Skipf("expected fixture %s missing — skip rather than guess at substitute", enrichTarget)
	}
	enriched := filepath.Join(t.TempDir(), "enriched.db")
	cmd = exec.Command("leyline", "lsp",
		"--server", "gopls",
		"--input", enrichTarget,
		"--output", enriched,
		"--merge-db", dbPath)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "leyline lsp failed: %s", out)
	dbPath = enriched

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // pin connection so TEMP views from ensureCanonicalViews persist
	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	withSkip, withoutSkip := runDeadCodeWithAndWithout(t, db)
	diff := stringSetDiff(withSkip, withoutSkip)

	t.Logf("Falsifiability A: %d findings with skip-list, %d without; %d newly flagged dead",
		len(withSkip), len(withoutSkip), len(diff))

	// For each newly-flagged def, look up _lsp_refs by token. If
	// LSP HAS a reference to that token, the design is broken —
	// the alive-check's binding arm should have made it alive.
	suspect := []string{}
	for _, nodeID := range diff {
		// Find this def's token via v_defs.
		var token string
		err := db.QueryRow(`SELECT token FROM v_defs WHERE node_id = ? AND fidelity = 'mention' LIMIT 1`, nodeID).Scan(&token)
		if err != nil {
			continue
		}
		// Does LSP have any binding ref for this exact node_id?
		var lspCount int
		err = db.QueryRow(
			`SELECT COUNT(*) FROM _lsp_refs WHERE node_id = ? AND ref_token != '' AND referrer_node_id IS NOT NULL`,
			nodeID,
		).Scan(&lspCount)
		if err != nil {
			continue
		}
		if lspCount > 0 {
			suspect = append(suspect, fmt.Sprintf("%s (token=%s, lsp_refs=%d)", nodeID, token, lspCount))
		}
	}
	assert.Empty(t, suspect,
		"Falsifiability A FAIL: LSP indexed these defs but ablation flagged them dead — view binding arm not working: %v",
		suspect)
}
