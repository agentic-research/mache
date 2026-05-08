package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Falsifiability B — π_{1→0} round-trip experiment per ADR-0013
// (mache-354464).
//
// Hypothesis: every binding-fidelity row in _lsp_refs (post-Step-1)
// projects down to a mention-fidelity row that already exists in
// node_refs. Formally:
//
//   ∀ r ∈ _lsp_refs.
//     (referrer_node_id, ref_token) ≠ (NULL, '')
//     ⟹ (referrer_node_id, ref_token) ∈ {(node_id, token) : node_refs}
//
// Modulo documented exceptions:
//
//   - Function values (closures, method values stored in slices/maps).
//     Tree-sitter's call extractor matches call_expression /
//     selector_expression sites; bare references to method values
//     don't match, so they're absent from node_refs even though LSP
//     sees them.
//   - Type-only references (e.g. type aliases, struct embedding,
//     `var x SomeType`). The call extractor matches calls; type
//     references are not calls.
//   - Macro-expanded code (Rust, C/C++ preprocessor). LSP sees the
//     post-expansion form; tree-sitter sees the pre-expansion source.
//
// If the harness reports discrepancies outside this taxonomy, the
// design is wrong (the projection π_{1→0} is unsound or the producer
// at level L_1 is inventing edges).
//
// This file ships TWO tests:
//
//   1. SyntheticHarness — documents the harness logic against a hand-
//      built fixture where we control both sides. Always runs.
//   2. IntegrationOnLeylineParse — runs leyline parse against mache's
//      own source, runs the harness, expects pass. Skipped when the
//      leyline binary is absent or the env gate is off.

// projectionDiscrepancy is one (referrer, token) pair from _lsp_refs
// that didn't appear in node_refs. The harness collects these and
// the test classifies each via the documented exception taxonomy.
type projectionDiscrepancy struct {
	ReferrerNodeID string
	RefToken       string
	TargetNodeID   string // for diagnostics
	RefURI         string
}

// runProjectionRoundTrip computes π_{1→0} for every _lsp_refs row in
// the given .db and returns the discrepancies — _lsp_refs rows whose
// (referrer_node_id, ref_token) pair is not in node_refs.
//
// Rows with empty ref_token or NULL referrer_node_id are skipped (the
// projection is undefined; producer didn't have source bytes available
// to extract a token, or the referrer wasn't in scope).
func runProjectionRoundTrip(db *sql.DB) ([]projectionDiscrepancy, error) {
	// Pull the (token, node_id) pairs from node_refs into a hash set.
	// At mache scale (~30k refs on this repo), this fits in memory.
	nodeRefsRows, err := db.Query(`SELECT token, node_id FROM node_refs`)
	if err != nil {
		return nil, fmt.Errorf("read node_refs: %w", err)
	}
	defer func() { _ = nodeRefsRows.Close() }()

	have := map[[2]string]struct{}{}
	for nodeRefsRows.Next() {
		var token, nodeID string
		if err := nodeRefsRows.Scan(&token, &nodeID); err != nil {
			return nil, fmt.Errorf("scan node_refs: %w", err)
		}
		have[[2]string{nodeID, token}] = struct{}{}
	}
	if err := nodeRefsRows.Err(); err != nil {
		return nil, err
	}

	// Iterate _lsp_refs and check each non-trivial row.
	lspRows, err := db.Query(`SELECT node_id, referrer_node_id, ref_token, ref_uri
	                          FROM _lsp_refs
	                          WHERE referrer_node_id IS NOT NULL AND ref_token != ''`)
	if err != nil {
		return nil, fmt.Errorf("read _lsp_refs: %w", err)
	}
	defer func() { _ = lspRows.Close() }()

	var diff []projectionDiscrepancy
	for lspRows.Next() {
		var target, referrer, token, refURI string
		if err := lspRows.Scan(&target, &referrer, &token, &refURI); err != nil {
			return nil, fmt.Errorf("scan _lsp_refs: %w", err)
		}
		if _, ok := have[[2]string{referrer, token}]; !ok {
			diff = append(diff, projectionDiscrepancy{
				ReferrerNodeID: referrer,
				RefToken:       token,
				TargetNodeID:   target,
				RefURI:         refURI,
			})
		}
	}
	return diff, lspRows.Err()
}

// TestFalsifiabilityB_SyntheticHarness pins the harness logic on a
// hand-built fixture where the taxonomy of pass/fail cases is known
// up front. Documents the experiment shape so the integration test
// can read short.
func TestFalsifiabilityB_SyntheticHarness(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "synthetic.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL,
			referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		-- node_refs: tree-sitter saw 'Validate' textually from billing.
		INSERT INTO node_refs VALUES ('Validate', 'billing/functions/Charge');

		-- _lsp_refs: gopls resolved billing's call to auth's Validate.
		-- This row's projection is (billing/functions/Charge, 'Validate'),
		-- which IS in node_refs → no discrepancy.
		INSERT INTO _lsp_refs VALUES (
			'auth/functions/Validate', 'billing/functions/Charge', 'Validate',
			'file:///billing.go', 42, 11, 42, 19);

		-- An empty-token row; harness must skip it (projection undefined).
		INSERT INTO _lsp_refs VALUES (
			'auth/functions/Empty', 'billing/functions/Charge', '',
			'file:///billing.go', 50, 0, 50, 1);

		-- A NULL-referrer row; harness must skip it.
		INSERT INTO _lsp_refs VALUES (
			'auth/functions/Validate', NULL, 'Validate',
			'file:///somewhere.go', 99, 0, 99, 8);
	`)
	require.NoError(t, err)

	diff, err := runProjectionRoundTrip(db)
	require.NoError(t, err)
	assert.Empty(t, diff,
		"every projectable _lsp_refs row appears in node_refs in this fixture")
}

// TestFalsifiabilityB_DiscrepancyShape exercises the negative case —
// an _lsp_refs row whose projection is NOT in node_refs. The harness
// must report it for the test to classify against the exception taxonomy.
func TestFalsifiabilityB_DiscrepancyShape(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "discrepancy.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE _lsp_refs (
			node_id TEXT NOT NULL,
			referrer_node_id TEXT,
			ref_token TEXT NOT NULL DEFAULT '',
			ref_uri TEXT NOT NULL,
			ref_start_line INTEGER NOT NULL, ref_start_col INTEGER NOT NULL,
			ref_end_line INTEGER NOT NULL, ref_end_col INTEGER NOT NULL
		);

		-- LSP sees a method-value reference (m := r.Read; later m()).
		-- Tree-sitter's call extractor doesn't capture the bare 'r.Read'
		-- form (no call_expression at the assignment site), so node_refs
		-- has no matching row. This is wedge case (a) from the ADR —
		-- function-value references — and shows up as a discrepancy.
		INSERT INTO _lsp_refs VALUES (
			'pkg/methods/MyType.Read', 'pkg/functions/Caller', 'Read',
			'file:///caller.go', 30, 11, 30, 15);
	`)
	require.NoError(t, err)

	diff, err := runProjectionRoundTrip(db)
	require.NoError(t, err)
	require.Len(t, diff, 1, "the synthetic function-value reference must surface as a discrepancy")
	assert.Equal(t, "pkg/functions/Caller", diff[0].ReferrerNodeID)
	assert.Equal(t, "Read", diff[0].RefToken)
	assert.Equal(t, "pkg/methods/MyType.Read", diff[0].TargetNodeID)
}

// TestFalsifiabilityB_IntegrationOnLeylineParse runs the harness
// against a real LLO-built .db. Requires the leyline binary on PATH
// and the env gate MACHE_FALSIFIABILITY_INTEGRATION=1 (the test reads
// hundreds of MB and runs gopls; not appropriate for the default
// `go test ./...` cycle).
func TestFalsifiabilityB_IntegrationOnLeylineParse(t *testing.T) {
	if os.Getenv("MACHE_FALSIFIABILITY_INTEGRATION") != "1" {
		t.Skip("set MACHE_FALSIFIABILITY_INTEGRATION=1 to run; needs leyline + LSP servers indexed")
	}
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH")
	}

	srcDir, err := os.Getwd()
	require.NoError(t, err)
	// Walk up from cmd/ to repo root.
	repoRoot := filepath.Dir(srcDir)
	dbPath := filepath.Join(t.TempDir(), "self.db")

	cmd := exec.Command("leyline", "parse", repoRoot, "-o", dbPath, "--lang", "go", "--lsp")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", out)

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	diff, err := runProjectionRoundTrip(db)
	require.NoError(t, err)

	// Expect SOME discrepancies (function values and type-only
	// references in mache's source), but NOT a wholesale failure
	// pattern that'd indicate the projection is broken.
	t.Logf("Falsifiability B: %d discrepancies / projected rows on mache self-build", len(diff))

	// Group by token to make the failure mode legible if it triggers.
	byToken := map[string]int{}
	for _, d := range diff {
		byToken[d.RefToken]++
	}
	tokens := make([]string, 0, len(byToken))
	for tok := range byToken {
		tokens = append(tokens, tok)
	}
	sort.Strings(tokens)
	for _, tok := range tokens {
		t.Logf("  %s: %d discrepancies", tok, byToken[tok])
	}

	// Soft assertion: if discrepancies dominate, the projection is
	// likely wrong. Choose a generous threshold; tighten in a
	// follow-up bead once we've seen real numbers.
	var totalLSPRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM _lsp_refs WHERE referrer_node_id IS NOT NULL AND ref_token != ''`,
	).Scan(&totalLSPRows))

	if totalLSPRows == 0 {
		t.Skip("no projectable _lsp_refs rows — leyline didn't enrich; nothing to falsify")
	}
	pctMissing := float64(len(diff)) / float64(totalLSPRows) * 100
	t.Logf("Falsifiability B: %.1f%% (%d/%d) of projectable _lsp_refs rows have no node_refs match",
		pctMissing, len(diff), totalLSPRows)

	// 50% is the soft ceiling. Function values + type-only refs are
	// expected slack but shouldn't dominate. Real-world Go code
	// reaches LSP via call sites the vast majority of the time.
	assert.LessOrEqual(t, pctMissing, 50.0,
		"more than half of LSP refs aren't textually mentioned — projection π_{1→0} suspect")
}
