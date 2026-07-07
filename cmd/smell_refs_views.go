package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/agentic-research/mache/internal/lsp"
)

// ensureCanonicalViews installs v_defs / v_refs (ADR-0013 Step 3) as
// TEMP views scoped to the active connection. Probes for ley-line-open's
// post-Step-1 _lsp_* columns (referrer_node_id, ref_token, def_token)
// and includes a UNION ALL with binding-fidelity rows when those
// columns are available; otherwise falls back to mention-only.
//
// Why TEMP views rather than persistent ones:
//
//   - SQLiteGraph opens .dbs read-only (mode=ro). Persistent DDL fails;
//     TEMP objects live in a per-connection in-memory schema and work
//     even on RO connections.
//   - The view body depends on what columns the producer wrote. With
//     TEMP views we can recompute the body per-connection rather than
//     locking in a stale shape on disk.
//   - Temp objects shadow same-named main-schema objects for the
//     current connection, so the legacy persistent mention-only views
//     installed by NewSQLiteWriter (PR #341) get overridden cleanly
//     without a migration.
//
// Idempotent — DROP VIEW IF EXISTS before each CREATE — so safe to
// call before every rule execution.
func ensureCanonicalViews(qg refsQuerier) error {
	// _lsp_defs probe still runs because v_defs's binding-fidelity
	// arm has not yet migrated to capnp — BindingRecord covers refs
	// (the link from a call site to a definition), not standalone
	// def metadata. _lsp_defs migration is tracked separately; for
	// now the def-side dual-read mirrors the pre-mache-6bd4d8 shape.
	hasLSPDefsToken, err := tableHasColumn(qg, "_lsp_defs", "def_token")
	if err != nil {
		return fmt.Errorf("probe _lsp_defs: %w", err)
	}

	// Unified code-fact IR (ADR-0023 step 2): when the db carries the IR
	// tables (ley-line-open's `fact_edges` + `symbols`), source the
	// mention arm from `fact_edges` instead of `node_defs`/`node_refs`.
	// The producer derives `fact_edges`' defines/references arms from the
	// same tree-sitter extraction that feeds `node_defs`/`node_refs`, so
	// the swap is byte-parity by construction; the probe falls back to the
	// legacy tables on dbs without the IR (older producers). `symbols`
	// carries `node_id`, so the join re-attaches the parse-run locator the
	// mention views expose. Same dual-read discipline as the T8.8 capnp
	// migration — probe the producer's shape, keep the consumer SQL fixed.
	// Requires both tables: an edges-without-symbols db falls back rather
	// than emit an empty (unjoinable) view.
	hasFactEdges, err := tableHasColumn(qg, "fact_edges", "kind")
	if err != nil {
		return fmt.Errorf("probe fact_edges: %w", err)
	}
	if hasFactEdges {
		hasSymbols, err := tableHasColumn(qg, "symbols", "node_id")
		if err != nil {
			return fmt.Errorf("probe symbols: %w", err)
		}
		hasFactEdges = hasSymbols
	}

	defsMentionArm := `SELECT token, node_id, 'mention' AS fidelity FROM node_defs`
	if hasFactEdges {
		defsMentionArm = `SELECT fe.token AS token, s.node_id AS node_id, 'mention' AS fidelity
			FROM fact_edges fe JOIN symbols s ON s.symbol_id = fe.src
			WHERE fe.kind = 'defines'`
	}

	defsBody := defsMentionArm
	if hasLSPDefsToken {
		defsBody += `
			UNION ALL
			SELECT def_token AS token, node_id, 'binding' AS fidelity
			FROM _lsp_defs
			WHERE def_token != ''`
	}

	// v_refs columns: referrer_node_id, token, target_node_id,
	// ref_uri, ref_line, fidelity, qualifier. The qualifier column
	// is empty for mention-fidelity rows (tree-sitter call extractor
	// doesn't populate it) and for pre-T8.7 capnp records (default
	// '' per schema-evolution invariant). See mache-6c0d07 for the
	// fan_out_skew metric that consumes it.
	refsMentionArm := `SELECT node_id AS referrer_node_id,
	       token,
	       NULL  AS target_node_id,
	       NULL  AS ref_uri,
	       NULL  AS ref_line,
	       'mention' AS fidelity,
	       ''    AS qualifier
	FROM node_refs`
	if hasFactEdges {
		// target_node_id is held NULL to match the node_refs mention shape
		// byte-for-byte. `fact_edges` DOES carry the resolved `dst`, but
		// surfacing it is a binding-fidelity enhancement for a later slice;
		// forcing NULL here keeps this a pure, provable substrate swap.
		refsMentionArm = `SELECT s.node_id AS referrer_node_id,
		       fe.token AS token,
		       NULL  AS target_node_id,
		       NULL  AS ref_uri,
		       NULL  AS ref_line,
		       'mention' AS fidelity,
		       ''    AS qualifier
		FROM fact_edges fe JOIN symbols s ON s.symbol_id = fe.src
		WHERE fe.kind = 'references'`
	}

	refsBody := refsMentionArm
	// Binding-fidelity rows come from the per-connection
	// _capnp_binding_refs TEMP table, populated from the sibling
	// .bindings.capnp event log by LoadCapnpBindings (mache-190508).
	// When LoadCapnpBindings hasn't run for this connection, the
	// table stays empty and v_refs surfaces only mention-fidelity
	// rows from node_refs — same as the pre-LSP behavior.
	//
	// The legacy _lsp_refs SQL UNION arm was retired in mache-6bd4d8
	// (T8.8 mirror): SQL columns are no longer the consumer-side
	// contract, the capnp event log is. Removing the arm structurally
	// precludes be6136-class column-name-as-protocol disagreements
	// between LLO writer and mache reader (rather than only
	// preventing them by flag). LLO continues writing _lsp_refs in
	// the transition window; mache no longer reads it.
	refsBody += `
		UNION ALL
		SELECT referrer_node_id,
		       token,
		       target_node_id,
		       ref_uri,
		       ref_line,
		       'binding' AS fidelity,
		       qualifier
		FROM _capnp_binding_refs`

	stmts := []string{
		"DROP VIEW IF EXISTS temp.v_defs",
		"DROP VIEW IF EXISTS temp.v_refs",
		"DROP TABLE IF EXISTS temp._capnp_binding_refs",
		// qualifier column added in mache-6c0d07 (T8.7 mirror).
		// Empty string when LLO didn't see a selector_expression
		// upstream of the ref site, OR when the record came from a
		// pre-T8.7 .bindings.capnp log. fan_out_skew uses
		// COALESCE(NULLIF(qualifier, ''), token) so both shapes
		// degrade gracefully.
		`CREATE TEMP TABLE _capnp_binding_refs (
			referrer_node_id TEXT NOT NULL,
			token TEXT NOT NULL,
			target_node_id TEXT,
			ref_uri TEXT,
			ref_line INTEGER,
			qualifier TEXT NOT NULL DEFAULT ''
		)`,
		"CREATE TEMP VIEW v_defs AS " + defsBody,
		"CREATE TEMP VIEW v_refs AS " + refsBody,
	}
	for _, s := range stmts {
		rows, err := qg.QueryRefs(s)
		if err != nil {
			return fmt.Errorf("ensure canonical views: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// LoadCapnpBindings populates the per-connection _capnp_binding_refs
// TEMP table from the sibling .bindings.capnp event log of dbPath.
// Must be called AFTER ensureCanonicalViews on the same connection
// (the TEMP table is created there).
//
// No-op when the sibling log is missing (returns nil) — the canonical
// view's UNION arm just stays empty. Returns an error when the log
// exists but is corrupt.
//
// Producers that write to BOTH _lsp_refs AND .bindings.capnp will
// produce duplicate-shaped rows in v_refs (one per producer); set-
// membership consumers (alive-check in dead_code) deduplicate
// naturally, so this isn't a correctness issue. Once the capnp event
// log is the canonical producer (post-T8.5), the _lsp_refs UNION arm
// in ensureCanonicalViews can be removed.
func LoadCapnpBindings(qg refsQuerier, dbPath string) error {
	if dbPath == "" {
		return nil
	}
	logPath := lsp.SiblingBindingLogPath(dbPath)
	records, err := lsp.ReadBindingLog(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load capnp bindings: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	// Single-row INSERTs are ~30K syscalls on a typical mache-scale
	// log. Acceptable for the first pass; if it shows up in profiles
	// switch to a multi-row INSERT or a prepared-stmt loop.
	for _, r := range records {
		stmt := `INSERT INTO _capnp_binding_refs
			(referrer_node_id, token, target_node_id, ref_uri, ref_line, qualifier)
			VALUES (?, ?, ?, ?, ?, ?)`
		rows, err := qg.QueryRefs(stmt, r.ConstructNodeID, r.RefToken,
			r.TargetNodeID, r.RefURI, int64(r.RefRange.StartLine), r.Qualifier)
		if err != nil {
			return fmt.Errorf("insert capnp binding: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// tableHasColumn returns true iff the given table exists AND contains
// a column of the given name. Implemented via PRAGMA table_info, which
// returns zero rows for missing tables (rather than erroring), so this
// helper collapses "table missing" and "column missing" into false.
//
// Used by ensureCanonicalViews to decide whether to add the binding-
// fidelity UNION ALL clause to the v_defs / v_refs body. A pre-Step-1
// _lsp_refs table (without referrer_node_id / ref_token) reads as
// "no binding-fidelity rows available" and the views fall back to
// mention-only — same shape as today.
func tableHasColumn(qg refsQuerier, table, col string) (bool, error) {
	// Table name is interpolated directly; PRAGMA table_info doesn't
	// accept positional parameters. table comes from a hardcoded
	// constant in this file (not user input), so injection risk is
	// nil — but assert anyway via a defensive check.
	if !isSimpleIdent(table) {
		return false, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := qg.QueryRefs(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		// SQLite returns rows (possibly zero) for valid PRAGMA calls
		// whether or not the table exists; an error here is genuine.
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// isSimpleIdent guards against SQL injection in PRAGMA table_info
// where the table name can't be parameterized. Allows ASCII letters,
// digits, and underscores — the shape of every table mache writes.
func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// dbPathProvider is the opt-in interface a refsQuerier implements
// when it knows its backing .db file path. The path is only used to
// locate the sibling .bindings.capnp event log for capnp-readthrough
// (mache-190508 step 3); queriers that don't know the path (in-memory
// test fixtures, the in-process arena) skip the capnp source
// silently. The mention + legacy SQL binding paths still apply.
type dbPathProvider interface {
	DBPath() string
}
