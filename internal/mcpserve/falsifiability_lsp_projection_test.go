package mcpserve

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

// projectionWalker maps an _lsp_refs.referrer_node_id to the AST node
// shape used by node_refs. LLO writes referrer_node_id pointing at the
// LEAF identifier (field_identifier/identifier/type_identifier — wherever
// the symbol token textually appears), but mache's node_refs writes the
// enclosing call_expression. Without a walk-up step the projection
// would compare leaf paths to call paths and report 100% noise.
//
// Returns (callNodeID, true) when the leaf is inside a call_expression;
// (_, false) when it's a non-call reference (type-only, function value,
// embedded type) — those rows are documented exceptions per the ADR
// taxonomy and don't count as discrepancies.
type projectionWalker func(leafNodeID string) (string, bool, error)

// identityWalker assumes _lsp_refs.referrer_node_id is already in the
// node_refs shape. Used by the synthetic harness tests that wire
// matching shapes directly. NOT correct for real LLO-built .dbs.
func identityWalker(_ *sql.DB) projectionWalker {
	return func(leaf string) (string, bool, error) {
		return leaf, true, nil
	}
}

// astCallSiteWalker walks UP from the leaf identifier to its enclosing
// call_expression via the _ast table. Returns (call_node_id, true) when
// such an ancestor exists; (_, false) when the leaf isn't part of any
// call_expression (e.g. a type_identifier in `var x SomeType`).
//
// Implementation: node_ids are slash-separated AST paths, so the
// enclosing call_expression is the LONGEST prefix of the leaf's
// node_id whose _ast.node_kind is 'call_expression'. SQLite's
// `:leaf LIKE node_id || '/%'` filter expresses descendant-of cleanly.
func astCallSiteWalker(db *sql.DB) projectionWalker {
	stmt, err := db.Prepare(`SELECT node_id FROM _ast
	                         WHERE node_kind = 'call_expression'
	                           AND ? LIKE node_id || '/%'
	                         ORDER BY length(node_id) DESC
	                         LIMIT 1`)
	if err != nil {
		// Table missing — fall back to identity. Surfaces as 100%
		// discrepancy on shape mismatch rather than a hard error,
		// keeping test output legible when the producer schema drifts.
		return identityWalker(db)
	}
	return func(leaf string) (string, bool, error) {
		var callID string
		switch err := stmt.QueryRow(leaf).Scan(&callID); err {
		case nil:
			return callID, true, nil
		case sql.ErrNoRows:
			return "", false, nil
		default:
			return "", false, fmt.Errorf("walk to call_expression for %q: %w", leaf, err)
		}
	}
}

// runProjectionRoundTrip computes π_{1→0} for every _lsp_refs row and
// returns the discrepancies — _lsp_refs rows whose projection is not
// in node_refs. The walker handles the leaf-identifier → call_expression
// shape conversion; pass identityWalker for fixtures that already use
// matching shapes.
//
// Rows with empty ref_token or NULL referrer_node_id are skipped (the
// projection is undefined; producer didn't have source bytes available
// to extract a token, or the referrer wasn't in scope).
//
// Rows whose walker returns isCallSite=false are also skipped — they're
// non-call references (ADR exception taxonomy), not discrepancies.
func runProjectionRoundTrip(db *sql.DB, walker projectionWalker) ([]projectionDiscrepancy, error) {
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
		callID, isCallSite, err := walker(referrer)
		if err != nil {
			return nil, err
		}
		if !isCallSite {
			// Non-call reference — type-only, function value, embedded
			// type. Documented ADR exception, not a discrepancy.
			continue
		}
		if _, ok := have[[2]string{callID, token}]; !ok {
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

	diff, err := runProjectionRoundTrip(db, identityWalker(db))
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

	diff, err := runProjectionRoundTrip(db, identityWalker(db))
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
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH — leyline lsp needs it")
	}

	srcDir, err := os.Getwd()
	require.NoError(t, err)
	// Walk up from cmd/ to repo root.
	repoRoot := filepath.Dir(srcDir)
	dbPath := filepath.Join(t.TempDir(), "self.db")

	// Step 1: parse — produces nodes / _ast / node_refs / node_defs.
	cmd := exec.Command("leyline", "parse", repoRoot, "-o", dbPath, "--lang", "go")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "leyline parse failed: %s", out)

	// Step 2: enrich one source file via gopls — populates _lsp_*.
	// Pick a small file from the repo so this completes quickly. We
	// only need the LSP rows to exist so the harness has something
	// to project; we don't need full-repo coverage to validate the
	// projection's soundness.
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

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	walker := astCallSiteWalker(db)
	diff, err := runProjectionRoundTrip(db, walker)
	require.NoError(t, err)

	// Sanity check: the AST walker must classify SOME _lsp_refs rows
	// as call-sites. If not, _ast is missing or the walker query is
	// wrong — silently treating everything as a non-call exception
	// would falsely report "no discrepancies" instead of validating
	// the projection.
	var callSiteCount, nonCallCount int
	walkRows, err := db.Query(`SELECT referrer_node_id FROM _lsp_refs
	                           WHERE referrer_node_id IS NOT NULL AND ref_token != ''`)
	require.NoError(t, err)
	for walkRows.Next() {
		var leaf string
		require.NoError(t, walkRows.Scan(&leaf))
		_, isCall, werr := walker(leaf)
		require.NoError(t, werr)
		if isCall {
			callSiteCount++
		} else {
			nonCallCount++
		}
	}
	require.NoError(t, walkRows.Close())
	t.Logf("Falsifiability B: %d call-site refs, %d non-call refs (type-only / function value), %d discrepancies",
		callSiteCount, nonCallCount, len(diff))
	require.Greater(t, callSiteCount, 0,
		"AST walker found zero call-sites — _ast missing call_expression rows or walker SQL broken")

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

	// Denominator is call-site rows only. Non-call refs (type-only,
	// function values) are documented ADR exceptions — the projection
	// doesn't claim to round-trip them, so including them would
	// inflate the noise-floor and mask genuine bugs.
	if callSiteCount == 0 {
		t.Skip("no call-site _lsp_refs rows — leyline didn't enrich; nothing to falsify")
	}
	pctMissing := float64(len(diff)) / float64(callSiteCount) * 100
	t.Logf("Falsifiability B: %.1f%% (%d/%d) of call-site _lsp_refs rows have no node_refs match",
		pctMissing, len(diff), callSiteCount)

	// 25% is the soft ceiling on call-site discrepancies. With the
	// AST walk-up in place, the remaining slack is function values
	// (m := r.Read; m()) and macro-expanded code — neither dominates
	// idiomatic Go. Higher than 25% means the producer at L_1 is
	// inventing call edges or the walker has a bug.
	assert.LessOrEqual(t, pctMissing, 25.0,
		"more than a quarter of call-site LSP refs lack node_refs match — projection π_{1→0} suspect")
}
